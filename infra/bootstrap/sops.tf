# KMS key for sops: human-authored secrets live in Git as secrets/<env>/*.enc.yaml
# with every value encrypted under this key (.sops.yaml). The secrets-sync
# workflow decrypts them and writes Secrets Manager; the runtime path
# (Secrets Manager → External Secrets → pod) does not change.
#
# Access model: whoever may decrypt this key may read every dev secret, so one
# key per environment (a prod key would admit only the release approvers and
# the sync role). The default key policy applies: the account can manage it,
# and IAM policies (the sync role's, the admins') decide who may use it.
resource "aws_kms_key" "sops" {
  description             = "sops encryption key for secrets/dev (eks-gitops-demo)"
  enable_key_rotation     = true
  deletion_window_in_days = 7 # demo: short window; production keeps 30
}

resource "aws_kms_alias" "sops" {
  name          = "alias/sops-demo"
  target_key_id = aws_kms_key.sops.key_id
}
