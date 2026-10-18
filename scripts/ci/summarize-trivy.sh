#!/usr/bin/env bash
# Prints a Markdown summary (counts by severity + findings) of a Trivy JSON report.
set -euo pipefail
title=$1 report=$2
echo "### $title"
echo
jq -r '[.Results[]? | (.Vulnerabilities // [])[], (.Misconfigurations // [])[] | .Severity]
       | group_by(.) | map("- \(.[0]): **\(length)**") | if length == 0 then ["No HIGH/CRITICAL findings."] else . end | .[]' "$report"
echo
jq -r '.Results[]? | .Target as $t
       | ((.Vulnerabilities // [])[] | "- `\($t)` \(.VulnerabilityID) \(.PkgName) \(.InstalledVersion) → \(.FixedVersion // "-") (\(.Severity))"),
         ((.Misconfigurations // [])[] | "- `\($t)` \(.ID) \(.Title) (\(.Severity))")' "$report" | head -50
echo
