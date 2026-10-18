# tflint: the bundled Terraform ruleset (naming, unused declarations, pinned
# providers and modules). Run per root: tflint --chdir infra/<root> --config "$PWD/.tflint.hcl"
plugin "terraform" {
  enabled = true
  preset  = "recommended"
}
