package registry

const AUTO_GENERATED_MSG string = `# Granted-Registry Autogenerated Section. DO NOT EDIT.
# This section is automatically generated by Granted (https://granted.dev). Manual edits to this section will be overwritten.
# To edit, clone your profile registry repo, edit granted.yml, and push your changes. You may need to make a pull request depending on the repository settings.
# To stop syncing and remove this section, run 'granted registry remove'.`

func GetAutogeneratedTemplate() string {
	return AUTO_GENERATED_MSG
}