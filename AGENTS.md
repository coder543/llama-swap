## Project Description:

llama-swap is a light weight, transparent proxy server that provides automatic model swapping to llama.cpp's server.

## Tech stack

- golang
- typescript, vite and svelt5 for UI (located in ui/)

## Workflow Tasks

- when summarizing changes only include details that require further action
- just say "Done." when there is no further action
- use the github CLI `gh` to create pull requests and work with github
- Rules for creating pull requests:
  - keep them short and focused on changes.
  - never include a test plan
  - write the summary using the same style rules as commit message

## Testing

- Follow test naming conventions like `TestProxyManager_<test name>`, `TestProcessGroup_<test name>`, etc.
- Use `go test -v -run <name pattern for new tests>` to run any new tests you've written.
- Run `gofmt -l .` before committing to verify formatting. Fix any reported files with `gofmt -w <file>`.
- Use `make test-dev` after running new tests for a quick over all test run. This runs `go test` and `staticcheck`. Fix any static checking errors. Use this only when changes are made to any code under the `proxy/` directory
- Use `make test-all` before completing work. This includes long running concurrency tests.
- Use `make test-ui` after making changes to the UI in ui-svelte/

## Aurelia and Cognicore Deployment

- Aurelia's `/home/coder/workspace/llama-swap/llama-swap-src` is the only maintained source checkout. Do not edit or build from a source checkout on Cognicore.
- After committing any changes in this repository, always build both Linux artifacts from Aurelia with `make linux-arm64 linux-amd64`. The embedded UI is built once and included in both binaries.
- Install the ARM64 artifact locally with `install -m 0755 build/llama-swap-linux-arm64 /home/coder/bin/llama-swap`.
- Aurelia uses the system-level `llama-swap.service`, running as `coder`. Restart it with `sudo systemctl restart llama-swap.service`.
- Verify Aurelia with `systemctl is-active llama-swap.service`, `curl -fsS http://127.0.0.1:8083/health`, and `curl -fsS http://127.0.0.1:8083/api/version`.
- Copy the AMD64 artifact to Cognicore as `/tmp/llama-swap-linux-amd64`, install it as `/home/coder/local-workspace/llama-swap/llama-swap`, remove the temporary copy, and restart Cognicore's user-level service with `systemctl --user restart llama-swap.service`.
- Verify Cognicore with `systemctl --user is-active llama-swap.service`, `curl -fsS http://127.0.0.1:8083/health`, and `curl -fsS http://127.0.0.1:8083/api/version`.
- Restarting either llama-swap service stops its managed model processes; the next model request may need to wait for a cold start.

### Commit message example format:

```
proxy: add new feature

Add new feature that implements functionality X and Y.

- key change 1
- key change 2
- key change 3

fixes #123
```

## Code Reviews

- use three levels High, Medium, Low severity
- label each discovered issue with a label like H1, M2, L3 respectively
- High severity are must fix issues (security, race conditions, critical bugs)
- Medium severity are recommended improvements (coding style, missing functionality, inconsistencies)
- Low severity are nice to have changes and nits
- Include a suggestion with each discovered item
- Limit your code review to three items with the highest priority first
- Double check your discovered items and recommended remediations
