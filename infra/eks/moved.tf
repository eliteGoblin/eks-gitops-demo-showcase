# State renames only (no destroy/create): the module's implicit "cluster
# creator" admin entry becomes the explicit "root" entry, and the standalone
# CI access entry moves into the module's access_entries map.
moved {
  from = module.eks.aws_eks_access_entry.this["cluster_creator"]
  to   = module.eks.aws_eks_access_entry.this["root"]
}

moved {
  from = module.eks.aws_eks_access_policy_association.this["cluster_creator_admin"]
  to   = module.eks.aws_eks_access_policy_association.this["root_admin"]
}

moved {
  from = aws_eks_access_entry.ci_terraform
  to   = module.eks.aws_eks_access_entry.this["ci_terraform"]
}

moved {
  from = aws_eks_access_policy_association.ci_terraform
  to   = module.eks.aws_eks_access_policy_association.this["ci_terraform_admin"]
}
