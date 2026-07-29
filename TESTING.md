# Testing the upgrade in AWS without touching the deployed (prod) version

This validates the upgraded kit end-to-end — the Go binaries (Phase 2), the merged
auth/monitoring/quota CloudFormation (Phase 1), and (optionally) the extracted Nexus
backend — in **isolation** from the currently deployed production environment.

## TL;DR

**Use a separate AWS account** (an AWS Organizations sandbox is easiest). It's the only
option that's safe by construction. Then deploy a **test profile** there, build the Go
binaries, install + run them in a **throwaway client** (container/VM), and tear down with
`ccwb destroy`. A same-account/different-region setup is possible but needs careful
resource renaming — see the fallback at the bottom and its hazards.

---

## Why isolation matters here (the hazards)

Testing in the same account as prod is dangerous because several resources are
account-global or shared by name:

1. **Model enforcement edits an IAM policy in place.** The Nexus API's `handle_update_models`
   resolves the `BedrockAccessPolicy` and calls `create_policy_version` on it. In the same
   account this would mutate **prod's** policy. (IAM is global — region isolation does **not**
   protect it.)
2. **DynamoDB tables default to fixed names** (`ClaudeCodeMetrics`, `QuotaPolicies`,
   `UserQuotaMetrics`, `NexusOrganizations`, `NexusDeviceCodes`). Same account + same names =
   shared/clobbered prod data unless every name is overridden.
3. **The distribution S3 bucket name is global** (`claude-code-auth-distribution-<account>`)
   and Cognito pools / IAM roles are account-scoped.
4. **Client-side clobber:** the generated `install.sh` writes `~/.claude/settings.json`
   (it backs up first), adds an AWS profile to `~/.aws/config`, and writes to the OS keyring.
   Running it on your own machine changes your local Claude Code / AWS config. Run it in a
   container or VM.

A separate account neutralizes 1–3 automatically; the container neutralizes 4.

---

## Recommended: separate AWS account

### 0. Point your shell at the test account
```bash
export AWS_PROFILE=nexus-sandbox      # creds for the SANDBOX account, not prod
aws sts get-caller-identity           # confirm it's the sandbox account id
```

### 1. Create a dedicated test profile (does not touch the prod profile)
```bash
cd source
poetry install
poetry run ccwb init --profile test
```
Choose values that are **distinct from prod**:
- a test **stack prefix / `identity_pool_name`** (e.g. `claudecode-test`) → all stack names
  become `claudecode-test-<auth|monitoring|quota>`, isolated from prod's;
- the test region;
- the test account's IdP / Cognito app config.

This writes a separate `test` profile into `config.json` alongside `ClaudeCode`.

### 2. Deploy the kit stacks to the sandbox
```bash
poetry run ccwb deploy --profile test
```
Deploys auth + monitoring + quota CloudFormation into the sandbox account/region under the
test stack names. Prod stacks are untouched (different account).

### 3. (Optional) Deploy the extracted Nexus backend
From `allcode-nexus-ui` (`import/nexus-backend` branch), deploy `infra/nexus-ui.yaml` etc. to
the sandbox. Until the deploy pipeline exists (Phase 3b), this is manual: upload the Lambda
zips to S3, deploy the stacks, and set the test profile's `device_auth_endpoint` /
`quota_api_endpoint` to the sandbox API Gateway URLs. If you're only validating the kit +
binaries, you can skip this and test browser-OIDC auth without the Nexus hub.

### 4. Build the Go binaries (Go is now the default)
```bash
poetry run ccwb package --profile test     # --go is the default now; add --pyinstaller only to test the legacy path
```
Produces the installer bundle under `dist/test/<timestamp>/` with cross-compiled Go binaries.

### 5. Install + run in a throwaway client (don't touch your machine)
Run the installer in a container so `~/.claude` / `~/.aws` / keyring stay clean:
```bash
docker run --rm -it -v "$PWD/dist/test/<timestamp>:/pkg" -w /pkg ubuntu:24.04 bash
#   inside: ./install.sh   (session storage avoids needing an OS keyring in the container)
```
Then exercise the flows from inside the container against the sandbox deployment:
- `aws sts get-caller-identity` via the credential process (auth → STS creds);
- a Bedrock `InvokeModel` / `Converse` call in an allowed region;
- if the Nexus hub is deployed: the **device-code flow**, **quota check**, **model
  enforcement** (set an `enabled_models` policy, confirm `ANTHROPIC_MODEL` is rewritten),
  and **platform reporting**;
- telemetry: confirm the otel-helper emits headers and (if monitoring deployed) the collector
  receives them.

### 6. Built-in validation
```bash
poetry run ccwb test --profile test            # auth + Bedrock access across representative regions
poetry run ccwb test --profile test --quota-only   # quota API / policies / usage capture
```

### 7. Tear down
```bash
poetry run ccwb destroy --profile test         # deletes the test stacks
poetry run ccwb cleanup --profile test         # removes residual/orphaned resources
```
Then delete the `test` profile from `config.json` if desired. (In a sandbox account you can
also just decommission the whole account.)

---

## Fallback: same account, different region (only if a sandbox account is impossible)

Use a different region **and** a distinct `identity_pool_name`, **and** override every
account-global name:
- DynamoDB table names (pass distinct `*_TABLE` env vars / CFN params for all five tables);
- the `BedrockAccessPolicy` name — **critical**: the model-enforcement code must target a
  **test-specific** policy, never prod's, or testing model enforcement will mutate prod;
- the distribution bucket name;
- distinct Cognito pool + IAM role names (stack-prefix driven).

Region isolation does **not** protect IAM or S3 (both global), so this path is fragile and
easy to get wrong. Prefer the separate account. If you must do this, dry-run every template
(`cfn-lint`, `aws cloudformation deploy --no-execute-changeset`) and review the changeset
before executing.

---

## What does NOT need an AWS deploy (already validated locally)

- Go unit tests, build, vet, and 5-platform cross-compile (Phase 2).
- The full Python test suite (896 tests after the Phase 3 extraction).
- These cover the binary logic; the AWS test above covers the integration (federation, Bedrock
  invoke, telemetry export, the Nexus device/quota flows) that unit tests can't.

> Tip: validate in the sandbox **before** doing Phase 2b step 2 (deleting the Python binaries)
> and before applying the identifier externalization — those are the changes whose correctness
> can only be confirmed by a real deployment.
