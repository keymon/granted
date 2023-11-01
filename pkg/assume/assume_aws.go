package assume

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/common-fate/awsconfigfile"
	"github.com/common-fate/clio"
	"github.com/common-fate/clio/clierr"
	"github.com/common-fate/granted/pkg/assumeprint"
	"github.com/common-fate/granted/pkg/browser"
	"github.com/common-fate/granted/pkg/cfaws"
	"github.com/common-fate/granted/pkg/config"
	"github.com/common-fate/granted/pkg/console"
	"github.com/common-fate/granted/pkg/forkprocess"
	"github.com/common-fate/granted/pkg/launcher"
	"github.com/common-fate/granted/pkg/testable"
	cfflags "github.com/common-fate/granted/pkg/urfav_overrides"
	"github.com/fatih/color"
	"github.com/hako/durafmt"
	"github.com/mgutz/ansi"
	"github.com/urfave/cli/v2"
	"gopkg.in/ini.v1"
)

type AssumeAWS struct {
	ctx         *cli.Context
	assumeFlags *cfflags.Flags
}

// processArgsAndExecFlag will return the profileName if provided and the exec command config if the exec flag is used
// this supports both the -- variant and the legacy flag when passes the command and args as a string for backwards compatability
func (a *AssumeAWS) processArgsAndExecFlag(c *cli.Context, assumeFlags *cfflags.Flags) (string, *execConfig, error) {
	execFlag := a.assumeFlags.String("exec")
	clio.Debugw("process args", "execFlag", execFlag, "osargs", os.Args, "c.args", c.Args().Slice())
	if execFlag == "" {
		return c.Args().First(), nil, nil
	}

	if execFlag == "--" {
		for i, arg := range os.Args {
			if arg == "--" {
				if len(os.Args) == i+1 {
					return "", nil, clierr.New("invalid arguments to exec call with '--'. Make sure you pass the command and argument after the doubledash.",
						clierr.Info("try running 'assume profilename --exec -- cmd arg1 arg2"))
				}
				cmdAndArgs := os.Args[i+1:]
				var args []string
				if len(cmdAndArgs) > 1 {
					args = cmdAndArgs[1:]
				}
				if c.Args().Len() > len(cmdAndArgs) {
					return c.Args().First(), &execConfig{cmdAndArgs[0], args}, nil
				} else {
					return "", &execConfig{cmdAndArgs[0], args}, nil
				}
			}
		}
	}

	parts := strings.SplitN(execFlag, " ", 2)
	var args []string
	if len(parts) > 1 {
		args = strings.Split(parts[1], " ")
	}
	return c.Args().First(), &execConfig{parts[0], args}, nil
}

func (a *AssumeAWS) Assume() error {

	if a.assumeFlags.String("exec") != "" && runtime.GOOS == "windows" {
		return clierr.New("--exec flag is not currently supported on Windows",
			clierr.Info("Let us know if you'd like support for this by creating an issue on our Github repo: https://github.com/common-fate/granted/issues/new"),
		)
	}

	profileName, execCfg, err := a.processArgsAndExecFlag(a.ctx, a.assumeFlags)
	if err != nil {
		return err
	}
	clio.Debug("processed profile name", profileName)
	clio.Debug("exec config:", execCfg)

	activeRoleProfile := a.assumeFlags.String("active-aws-profile")
	activeRoleFlag := a.assumeFlags.Bool("active-role")

	showRerunCommand := false
	var profile *cfaws.Profile
	if a.assumeFlags.Bool("sso") {
		profile, err = SSOProfileFromFlags(a.ctx)
		if err != nil {
			return err
		}

		// save the profile to the AWS config file if the user requested it.
		saveProfileName := a.assumeFlags.String("save-to")
		if saveProfileName != "" {
			configFilename := cfaws.GetAWSConfigPath()
			config, err := ini.LoadSources(ini.LoadOptions{
				AllowNonUniqueSections:  false,
				SkipUnrecognizableLines: false,
				AllowNestedValues:       true,
			}, configFilename)
			if err != nil {
				if !os.IsNotExist(err) {
					return err
				}
				config = ini.Empty()
			}
			err = awsconfigfile.Merge(awsconfigfile.MergeOpts{
				Config:              config,
				SectionNameTemplate: saveProfileName,
				Profiles: []awsconfigfile.SSOProfile{
					{
						SSOStartURL:   profile.AWSConfig.SSOStartURL,
						SSORegion:     profile.AWSConfig.SSORegion,
						AccountID:     profile.AWSConfig.SSOAccountID,
						AccountName:   profile.AWSConfig.SSOAccountID,
						RoleName:      profile.AWSConfig.SSORoleName,
						GeneratedFrom: "commonfate",
					},
				},
			})
			if err != nil {
				return err
			}

			err = config.SaveTo(configFilename)
			if err != nil {
				return err
			}

			clio.Successf("Saved AWS profile as %s. You can use this profile with the AWS CLI using the '--profile' flags when running AWS commands.", saveProfileName)
		}
	} else if activeRoleFlag && os.Getenv("GRANTED_SSO") == "true" {
		profile, err = SSOProfileFromEnv()
		if err != nil {
			return err
		}
	} else {
		var wg sync.WaitGroup

		withStdio := survey.WithStdio(os.Stdin, os.Stderr, os.Stderr)

		profiles, err := cfaws.LoadProfiles()
		if err != nil {
			return err
		}

		if profileName != "" {
			if !profiles.HasProfile(profileName) {
				clio.Warnf("%s does not match any profiles in your AWS config or credentials files", profileName)
				profileName = ""
			}
		}

		//set the session creds using the active role if we have one and the flag is set
		if activeRoleFlag && activeRoleProfile != "" {
			if !profiles.HasProfile(activeRoleProfile) {
				clio.Warnf("You tried to use the -active-role flag but %s does not match any profiles in your AWS config or credentials files", activeRoleProfile)
			} else {
				profileName = activeRoleProfile
				clio.Infof("Using active profile: %s", profileName)
			}
		}
		if profileName != "" {
			// background task to update the frecency cache
			wg.Add(1)
			go func() {
				cfaws.UpdateFrecencyCache(profileName)
				wg.Done()
			}()
		}

		// if profile is still "" here, then prompt to select a profile
		if profileName == "" {
			// will print a command output for the user so it's easier for them to re-run later or learn the commands
			showRerunCommand = true
			// load config to check frecency enabled
			cfg, err := config.Load()
			if err != nil {
				return err
			}

			fr, profileNames := profiles.GetFrecentProfiles()
			if cfg.Ordering == "Alphabetical" {
				profileNames = profiles.ProfileNames
			}
			profileNameMap := make(map[string]string)
			profileKeys := make([]string, len(profileNames))
			var longestProfileNameLength int
			for _, pn := range profileNames {
				if len(pn) > longestProfileNameLength {
					longestProfileNameLength = len(pn)
				}
			}
			lightBlack := ansi.ColorFunc(ansi.LightBlack)
			var hasDescriptions bool
			for i, pn := range profileNames {
				var description string
				p, _ := profiles.Profile(pn)

				if p != nil && p.CustomGrantedProperty("description") != "" {
					hasDescriptions = true
					description = p.CustomGrantedProperty("description")
				}

				stringKey := fmt.Sprintf("%-"+strconv.Itoa(longestProfileNameLength)+"s%s", pn, lightBlack(description))

				profileNameMap[stringKey] = pn
				profileKeys[i] = stringKey
			}
			var promptHeader string
			// only add the description headers if there are profiles using descriptions
			if hasDescriptions {
				promptHeader = fmt.Sprintf(`{{- "  %s\n"}}`, color.New(color.Underline, color.Bold).Sprintf("%-"+strconv.Itoa(longestProfileNameLength)+"s%s", "Profile", "Description"))
			}
			// This overrides the default prompt template to add a header row above the options
			// this should be reset back to the original template after the call to AskOne
			originalSelectTemplate := survey.SelectQuestionTemplate
			survey.SelectQuestionTemplate = fmt.Sprintf(`
{{- define "option"}}
	{{- if eq .SelectedIndex .CurrentIndex }}{{color .Config.Icons.SelectFocus.Format }}{{ .Config.Icons.SelectFocus.Text }} {{else}}{{color "default"}}  {{end}}
	{{- .CurrentOpt.Value}}
{{- color "reset"}}
{{end}}
{{- if .ShowHelp }}{{- color .Config.Icons.Help.Format }}{{ .Config.Icons.Help.Text }} {{ .Help }}{{color "reset"}}{{"\n"}}{{end}}
{{- color .Config.Icons.Question.Format }}{{ .Config.Icons.Question.Text }} {{color "reset"}}
{{- color "default+hb"}}{{ .Message }}{{ .FilterMessage }}{{color "reset"}}
{{- if .ShowAnswer}}{{color "cyan"}} {{.Answer}}{{color "reset"}}{{"\n"}}
{{- else}}
  {{- "  "}}{{- color "cyan"}}[Use arrows to move, type to filter{{- if and .Help (not .ShowHelp)}}, {{ .Config.HelpInput }} for more help{{end}}]{{color "reset"}}
  {{- "\n"}}
  %s
  {{- range $ix, $option := .PageEntries}}
	{{- template "option" $.IterateOption $ix $option}}
  {{- end}}
{{- end}}`, promptHeader)

			clio.NewLine()
			// Replicate the logic from original assume fn.
			in := survey.Select{
				Message: "Please select the profile you would like to assume:",
				Options: profileKeys,
				Filter:  filterMultiToken,
			}
			if len(profileKeys) == 0 {
				return clierr.New("Granted couldn't find any AWS profiles in your config file or your credentials file",
					clierr.Info("You can add profiles to your AWS config by following our guide: "),
					clierr.Info("https://docs.commonfate.io/granted/getting-started#set-up-your-aws-profile-file"),
				)
			}

			err = testable.AskOne(&in, &profileName, withStdio)
			if err != nil {
				return err
			}
			// Reset the template for select questions to the original
			survey.SelectQuestionTemplate = originalSelectTemplate
			profileName = profileNameMap[profileName]
			// background task to update the frecency cache
			wg.Add(1)
			go func() {
				fr.Update(profileName)
				wg.Done()
			}()
		}
		// ensure that frecency has finished updating before returning from this function
		defer wg.Wait()
		// finally, load the profile and initialise it, this builds the parent tree structure
		profile, err = profiles.LoadInitialisedProfile(a.ctx.Context, profileName)
		if err != nil {
			return err
		}
	}

	var region string
	// The region flag may be supplied in shorthand form, first check if the flag is set and expand the region
	// else use the profile region
	if a.assumeFlags.String("region") != "" {
		regionFlag := a.assumeFlags.String("region")
		region, err = cfaws.ExpandRegion(regionFlag)
		if err != nil {
			return fmt.Errorf("couldn't parse region %s: %v", region, err)
		}
	} else {
		region, err = profile.Region(a.ctx.Context)
		if err != nil {
			return err
		}
	}

	configOpts := cfaws.ConfigOpts{Duration: time.Hour, MFATokenCode: ""}

	if a.assumeFlags.String("mfa-token") != "" {
		configOpts.MFATokenCode = a.assumeFlags.String("mfa-token")
	}

	// attempt to get session duration from profile
	if profile.AWSConfig.RoleDurationSeconds != nil {
		configOpts.Duration = *profile.AWSConfig.RoleDurationSeconds
	}

	duration := a.assumeFlags.String("duration")
	if duration != "" {
		d, err := time.ParseDuration(duration)
		if err != nil {
			return err
		}
		configOpts.Duration = d
	}

	if len(a.assumeFlags.StringSlice("pass-through")) > 0 {
		configOpts.Args = a.assumeFlags.StringSlice("pass-through")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// if getConsoleURL is true, we'll use the AWS federated login to retrieve a URL to access the console.
	// depending on how Granted is configured, this is then printed to the terminal or a browser is launched at the URL automatically.
	getConsoleURL := !a.assumeFlags.Bool("env") && ((a.assumeFlags.Bool("console") || a.assumeFlags.String("console-destination") != "") || a.assumeFlags.Bool("active-role") || a.assumeFlags.String("service") != "" || a.assumeFlags.Bool("url") || a.assumeFlags.String("browser-profile") != "")

	// this makes it easy for users to copy the actual command and avoid needing to lookup profiles
	if !cfg.DisableUsageTips && showRerunCommand {
		clio.Infof("To assume this profile again later without needing to select it, run this command:\n> assume %s %s", profile.Name, strings.Join(os.Args[1:], " "))
	}

	if getConsoleURL {
		con := console.AWS{
			Profile:     profile.Name,
			Service:     a.assumeFlags.String("service"),
			Region:      region,
			Destination: a.assumeFlags.String("console-destination"),
		}

		creds, err := profile.AssumeConsole(a.ctx.Context, configOpts)
		if err != nil {
			return err
		}

		containerProfile := profile.Name

		if a.assumeFlags.String("browser-profile") != "" {
			containerProfile = a.assumeFlags.String("browser-profile")
		}

		consoleURL, err := con.URL(creds)
		if err != nil {
			return err
		}

		if cfg.DefaultBrowser == browser.FirefoxKey || cfg.DefaultBrowser == browser.WaterfoxKey || cfg.DefaultBrowser == browser.FirefoxStdoutKey {
			// tranform the URL into the Firefox Tab Container format.
			consoleURL = fmt.Sprintf("ext+granted-containers:name=%s&url=%s&color=%s&icon=%s", containerProfile, url.QueryEscape(consoleURL), profile.CustomGrantedProperty("color"), profile.CustomGrantedProperty("icon"))
		}

		justPrintURL := a.assumeFlags.Bool("url") || cfg.DefaultBrowser == browser.StdoutKey || cfg.DefaultBrowser == browser.FirefoxStdoutKey
		if justPrintURL {
			// return the url via stdout through the CLI wrapper script and return early.
			fmt.Print(assumeprint.SafeOutput(consoleURL))
			return nil
		}

		browserPath := cfg.CustomBrowserPath
		if browserPath == "" {
			return errors.New("default browser not configured. run `granted browser set` to configure")
		}

		var l Launcher
		switch cfg.DefaultBrowser {
		case browser.ChromeKey, browser.BraveKey, browser.EdgeKey, browser.ChromiumKey:
			l = launcher.ChromeProfile{
				BrowserType:    cfg.DefaultBrowser,
				ExecutablePath: browserPath,
			}
		case browser.FirefoxKey, browser.WaterfoxKey:
			l = launcher.Firefox{
				ExecutablePath: browserPath,
			}
		case browser.SafariKey:
			l = launcher.Safari{}
		case browser.ArcKey:
			l = launcher.Arc{}
		case browser.FirefoxDevEditionKey:
			l = launcher.FirefoxDevEdition{
				ExecutablePath: browserPath,
			}
		case browser.CommonFateKey:
			l = launcher.CommonFate{
				// for CommonFate, executable path must be set as custom browser path
				ExecutablePath: browserPath,
			}
		default:
			l = launcher.Open{}
		}

		printFlagUsage(con.Region, con.Service)
		clio.Infof("Opening a console for %s in your browser...", profile.Name)

		// now build the actual command to run - e.g. 'firefox --new-tab <URL>'
		args := l.LaunchCommand(consoleURL, containerProfile)

		var startErr error
		if l.UseForkProcess() {
			clio.Debugf("running command using forkprocess: %s", args)
			cmd, err := forkprocess.New(args...)
			if err != nil {
				return err
			}
			startErr = cmd.Start()
		} else {
			clio.Debugf("running command without forkprocess: %s", args)
			cmd := exec.Command(args[0], args[1:]...)
			startErr = cmd.Start()
		}

		if startErr != nil {
			return clierr.New(fmt.Sprintf("Granted was unable to open a browser session automatically due to the following error: %s", startErr.Error()),
				// allow them to try open the url manually
				clierr.Info("You can open the browser session manually using the following url:"),
				clierr.Info(consoleURL),
			)
		}
	}

	// check if it's needed to provide credentials to terminal or default to it if console wasn't specified
	if a.assumeFlags.Bool("terminal") || !getConsoleURL {
		creds, err := profile.AssumeTerminal(a.ctx.Context, configOpts)
		if err != nil {
			return err
		}
		sessionExpiration := ""
		if creds.CanExpire {
			sessionExpiration = creds.Expires.Local().Format(time.RFC3339)
			// We add 10 seconds here as a fudge factor, the credentials will be a
			// few seconds old already.
			durationDescription := durafmt.Parse(time.Until(creds.Expires) + 10*time.Second).LimitFirstN(1).String()
			if os.Getenv("GRANTED_QUIET") != "true" {
				clio.Successf("[%s](%s) session credentials will expire in %s", profile.Name, region, durationDescription)
			}
		} else if os.Getenv("GRANTED_QUIET") != "true" {
			clio.Successf("[%s](%s) session credentials ready", profile.Name, region)
		}
		if a.assumeFlags.Bool("env") {
			err = cfaws.WriteCredentialsToDotenv(region, creds)
			if err != nil {
				return err
			}
			clio.Success("Exported credentials to .env file successfully")
		}

		if a.assumeFlags.Bool("export") {
			err = cfaws.ExportCredsToProfile(profile.Name, creds)
			if err != nil {
				return err
			}
			var profileName string
			if cfg.ExportCredentialSuffix != "" {
				profileName = profile.Name + "-" + cfg.ExportCredentialSuffix

			} else {
				profileName = profile.Name
				clio.Warn("No credential suffix found. This can cause issues with using exported credentials if conflicting profiles exist. Run `granted settings export-suffix set` to set one.")
			}

			clio.Successf("Exported credentials to ~/.aws/credentials file as %s successfully", profileName)
		}

		if execCfg != nil {
			return RunExecCommandWithCreds(creds, region, execCfg.Cmd, execCfg.Args...)
		}

		if profile.RawConfig.HasKey("credential_process") && a.assumeFlags.Bool("export-all-env-vars") {
			canExpire := "false"
			if creds.CanExpire {
				canExpire = "true"
			}
			output := PrepareStringsForShellScript([]string{creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, profile.Name, region, sessionExpiration, canExpire, profile.AWSConfig.SSOStartURL, profile.AWSConfig.SSORoleName, profile.AWSConfig.SSORegion, profile.AWSConfig.SSOAccountID})
			fmt.Printf("GrantedAssume %s %s %s %s %s %s %s %s %s %s %s", output...)

			return nil
		}

		// DO NOT REMOVE, this interacts with the shell script that wraps the assume command, the shell script is what configures your shell environment vars
		// to export more environment variables, add then in the assume and assume.fish scripts then append them to this output preparation function
		// the shell script treats "None" as an empty string and will not set a value for that positional output

		// If the profile uses "credential_process" to source credential externally then do not set accessKeyId, secretAccessKey, sessionToken
		// so that aws cli automatically refreshes credential when they expire.
		if profile.RawConfig.HasKey("credential_process") {
			output := PrepareStringsForShellScript([]string{"", "", "", profile.Name, region, "", "true", "", "", "", ""})
			fmt.Printf("GrantedAssume %s %s %s %s %s %s %s %s %s %s %s", output...)

			return nil
		}

		if a.assumeFlags.Bool("sso") {
			output := PrepareStringsForShellScript([]string{creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, "", region, sessionExpiration, "true", profile.AWSConfig.SSOStartURL, profile.AWSConfig.SSORoleName, profile.AWSConfig.SSORegion, profile.AWSConfig.SSOAccountID})
			fmt.Printf("GrantedAssume %s %s %s %s %s %s %s %s %s %s %s", output...)

			return nil
		}

		output := PrepareStringsForShellScript([]string{creds.AccessKeyID, creds.SecretAccessKey, creds.SessionToken, profile.Name, region, sessionExpiration, "false", "", "", "", ""})
		fmt.Printf("GrantedAssume %s %s %s %s %s %s %s %s %s %s %s", output...)
	}

	return nil
}