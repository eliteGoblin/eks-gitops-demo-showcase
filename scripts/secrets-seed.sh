#!/usr/bin/env bash
# Seed the long-lived secrets once (idempotent: existing values are kept).
# Values are generated here and written straight to AWS Secrets Manager or
# GitHub, so they never touch Terraform state or Git.
#   - demo/keycloak/admin      Keycloak bootstrap admin (master realm)
#   - demo/argocd/oidc         Argo CD OIDC client secret (Keycloak client "argocd")
#   - demo/argocd/gitops-repo  read-only deploy key Argo CD uses to pull the gitops repo
#   - demo/load-generator      Keycloak client secret for the k6 load generator (client credentials)
#   - demo/demo-api-svc        Keycloak client secret of demo-api's service account (calls payments-svc)
#   - demo/grafana/admin       Grafana built-in admin (break-glass and API): username, password
#   - demo/grafana/oidc        Grafana OIDC client secret (Keycloak client "grafana")
#   - demo/keycloak/users      demo users' passwords (realm demo): {"<username>": "<password>", ...}
#   - GITOPS_DEPLOY_KEY        write deploy key for CI promotion (app-repo Actions secret)
# The Grafana containers are created by infra/eks (the others by infra/bootstrap),
# so `make eks-up` runs this after the eks apply; run alone, it skips containers
# that do not exist yet. Without these values the Keycloak realm Job cannot start
# (it needs the grafana client secret), which takes every realm client down.
# ROTATE_DEMO_PASSWORDS=1 replaces the demo users' passwords (README: "Live demo on EKS").
set -euo pipefail
REGION=${AWS_REGION:-ap-southeast-2}
APP_REPO=${APP_REPO:-eliteGoblin/eks-gitops-demo}
GITOPS_REPO=${GITOPS_REPO:-eliteGoblin/eks-gitops-demo-gitops}

has_value() { aws secretsmanager get-secret-value --region "$REGION" --secret-id "$1" >/dev/null 2>&1; }
has_container() { aws secretsmanager describe-secret --region "$REGION" --secret-id "$1" >/dev/null 2>&1; }
put_value() { aws secretsmanager put-secret-value --region "$REGION" --secret-id "$1" --secret-string "$2" >/dev/null; echo "seeded $1"; }
random() { openssl rand -base64 48 | tr -dc 'A-Za-z0-9' | head -c "$1"; }

# Demo users' passwords: memorable but random, Word-Word-NNNN-symbol from /dev/urandom
# (2 of 256 words, 4 digits, 1 of 8 symbols: about 32 bits, behind Keycloak's brute-force
# lockout). Every character stays literal in YAML, in keycloak-config-cli's $(env:...)
# substitution and in a form post. The users are those of the gitops repo's realm.yaml
# (one entry each in config/keycloak/user-passwords.yaml).
DEMO_USERS="zhaolin.sun case.officer demo.guest sam.nominee riley.provider"
WORDS=(
  acorn almond alpine amber anchor antler apple apricot arrow aspen atlas autumn avocado badge bagel balloon
  bamboo banjo barley basil beacon beetle berry birch biscuit bison blanket blossom bonsai boulder bramble breeze
  brick bridge brook bucket buffalo button cabin cable cactus camel candle canoe canyon caramel carbon carrot
  cashew castle cedar cello chalk cheetah cherry chimney cider cinder citrus cliff cloud clover cobalt coconut
  comet compass cookie copper coral cotton cougar crane crater crayon cricket crystal daisy delta desert dolphin
  donkey dragon dune eagle ember emerald falcon feather fennel ferry fiddle fiesta finch flame flannel fleece
  flint forest fossil gadget galaxy garden garnet gecko ginger glacier globe goose gopher granite grape gravel
  hammer hamster harbor harvest hazel hedge helmet heron hollow honey horizon igloo island ivory jacket jaguar
  jigsaw jungle kayak kernel kettle koala ladder lagoon lantern laurel lemon linen lizard lobster lotus magnet
  mango maple marble marlin meadow melon meteor mirror moose mosaic muffin napkin nectar needle nickel noodle
  nutmeg oasis ocean olive orange orbit orchid otter oyster paddle panda parrot parsley peanut pebble pelican
  penguin pepper piano pickle pigeon pillow planet pocket polar pollen poppy prairie pretzel prism puffin pumpkin
  puzzle quartz quill rabbit radar radish raisin raven reef ribbon river robin rocket saddle salmon sandal
  sardine satin scarf sesame shell silver sketch sparrow spinach sprout spruce squash summit sunset swallow tablet
  tango teacup teapot thunder tiger timber tomato topaz tornado toucan tractor trumpet tulip tundra turnip turtle
  valley velvet violet violin volcano waffle walnut walrus willow window winter wizard wombat yogurt zebra zephyr
)
SYMBOLS='!@#*?=_.'
below() { # n -> uniform integer in [0, n), n <= 65536 (rejection sampling on 16 random bits)
  local lim=$(( 65536 - 65536 % $1 )) r
  while :; do
    r=$(od -An -N2 -tu2 /dev/urandom | tr -d ' ')
    case $r in ''|*[!0-9]*) return 1 ;; esac
    if [ "$r" -lt "$lim" ]; then echo $(( r % $1 )); return; fi
  done
}
# Fail closed: a broken random source must stop the script, never yield a fixed password.
word() { local i w; i=$(below ${#WORDS[@]}) || return 1; w=${WORDS[$i]}; printf '%s%s' "$(printf '%s' "${w:0:1}" | tr '[:lower:]' '[:upper:]')" "${w:1}"; }
demo_password() {
  local a b n s
  a=$(word) && b=$(word) && n=$(below 10000) && s=$(below ${#SYMBOLS}) || return 1
  printf '%s-%s-%04d-%s' "$a" "$b" "$n" "${SYMBOLS:$s:1}"
}

has_value demo/keycloak/admin || put_value demo/keycloak/admin \
  "$(jq -cn --arg p "$(random 32)" '{username:"admin",password:$p}')"
has_value demo/argocd/oidc || put_value demo/argocd/oidc \
  "$(jq -cn --arg s "$(random 40)" '{clientSecret:$s}')"
has_value demo/load-generator || put_value demo/load-generator \
  "$(jq -cn --arg s "$(random 40)" '{clientSecret:$s}')"
has_value demo/demo-api-svc || put_value demo/demo-api-svc \
  "$(jq -cn --arg s "$(random 40)" '{clientSecret:$s}')"
for name in demo/grafana/admin demo/grafana/oidc; do
  if ! has_container "$name"; then echo "skip $name (container not created yet: apply infra/eks first)"; continue; fi
  case $name in
    */admin) has_value "$name" || put_value "$name" "$(jq -cn --arg p "$(random 32)" '{username:"admin",password:$p}')" ;;
    */oidc)  has_value "$name" || put_value "$name" "$(jq -cn --arg s "$(random 40)" '{clientSecret:$s}')" ;;
  esac
done

tmp=$(mktemp -d); trap 'rm -rf "$tmp"' EXIT
if [ "${ROTATE_DEMO_PASSWORDS:-}" = 1 ] || ! has_value demo/keycloak/users; then
  has_container demo/keycloak/users || { echo "demo/keycloak/users does not exist: apply infra/bootstrap first" >&2; exit 1; }
  # Generated into a file and sent from it: no password on a command line or in a log.
  : > "$tmp/users.tsv"
  for u in $DEMO_USERS; do
    p=$(demo_password) || { echo "random source failed; nothing written" >&2; exit 1; }
    printf '%s\t%s\n' "$u" "$p" >> "$tmp/users.tsv"
  done
  jq -Rn '[inputs | split("\t") | {(.[0]): .[1]}] | add' < "$tmp/users.tsv" > "$tmp/users.json"
  jq -e --arg users "$DEMO_USERS" '(keys == ($users | split(" ") | sort)) and ([.[]] | unique | length) == length
    and all(.[]; test("^[A-Z][a-z]{3,6}-[A-Z][a-z]{3,6}-[0-9]{4}-[!@#*?=_.]$"))' "$tmp/users.json" >/dev/null \
    || { echo "generated demo passwords failed validation; nothing written" >&2; exit 1; }
  aws secretsmanager put-secret-value --region "$REGION" --secret-id demo/keycloak/users \
    --secret-string "file://$tmp/users.json" >/dev/null
  echo "seeded demo/keycloak/users"
fi
if ! has_value demo/argocd/gitops-repo; then
  ssh-keygen -q -t ed25519 -N '' -C argocd-readonly -f "$tmp/ro"
  gh repo deploy-key add "$tmp/ro.pub" --repo "$GITOPS_REPO" --title argocd-readonly
  put_value demo/argocd/gitops-repo "$(jq -cn --rawfile k "$tmp/ro" '{sshPrivateKey:$k}')"
fi
if ! gh secret list --repo "$APP_REPO" | grep -q '^GITOPS_DEPLOY_KEY'; then
  ssh-keygen -q -t ed25519 -N '' -C ci-promote -f "$tmp/rw"
  gh repo deploy-key add "$tmp/rw.pub" --repo "$GITOPS_REPO" --title ci-promote --allow-write
  gh secret set GITOPS_DEPLOY_KEY --repo "$APP_REPO" < "$tmp/rw"
  echo "seeded GITOPS_DEPLOY_KEY"
fi
echo "secrets ready"
