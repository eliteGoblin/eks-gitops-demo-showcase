# Postgres for Keycloak now (database "keycloak") and the app store in Phase 3.
# RDS generates the master password and keeps it in Secrets Manager
# (manage_master_user_password), rotating it on its own schedule; Terraform
# never sees the value. Keycloak reads it through external-secrets, and
# Reloader restarts Keycloak when it rotates.
resource "aws_db_subnet_group" "this" {
  name       = var.name
  subnet_ids = module.vpc.private_subnets
}

resource "aws_security_group" "rds" {
  name        = "${var.name}-rds"
  description = "Postgres from the EKS nodes and pods only"
  vpc_id      = module.vpc.vpc_id
}

# Pods use the node's security groups (VPC CNI without security groups for pods).
resource "aws_vpc_security_group_ingress_rule" "rds_from_cluster" {
  for_each = {
    node    = module.eks.node_security_group_id
    primary = module.eks.cluster_primary_security_group_id
  }
  security_group_id            = aws_security_group.rds.id
  referenced_security_group_id = each.value
  ip_protocol                  = "tcp"
  from_port                    = 5432
  to_port                      = 5432
  description                  = "Postgres from ${each.key} security group"
}

resource "aws_db_instance" "this" {
  identifier     = var.name
  engine         = "postgres"
  engine_version = "17"
  instance_class = "db.t4g.micro"

  allocated_storage = 20
  storage_type      = "gp3"
  storage_encrypted = true

  db_name                     = "keycloak"
  username                    = "dbadmin"
  manage_master_user_password = true

  db_subnet_group_name   = aws_db_subnet_group.this.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  publicly_accessible    = false
  multi_az               = false

  backup_retention_period = 1
  skip_final_snapshot     = true # demo: destroyed with the cluster
  deletion_protection     = false
  apply_immediately       = true
}
