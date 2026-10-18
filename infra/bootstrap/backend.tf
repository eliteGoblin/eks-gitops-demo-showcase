# Bootstrap: the first apply runs with local state (comment this block out),
# then `terraform init -migrate-state` moves the state into the bucket it created.
terraform {
  backend "s3" {
    bucket       = "eks-gitops-demo-tfstate-852544369865"
    key          = "bootstrap/terraform.tfstate"
    region       = "ap-southeast-2"
    encrypt      = true
    use_lockfile = true # S3-native locking; no DynamoDB table needed
  }
}
