# Releasing ktunnel

Cutting a release is a single tag push. Everything after that is automated
by `.github/workflows/release.yaml`.

Read the [Required secrets](#required-secrets) section first — two of the
four secrets have to be created by a maintainer, and without them a
release still *succeeds* while quietly failing to publish.

## Required secrets

Set these under **Settings → Secrets and variables → Actions → New
repository secret**.

| Secret | Required | Provided by | What breaks without it |
| --- | --- | --- | --- |
| `GITHUB_TOKEN` | — | GitHub, automatically | Nothing. Never create this one. |
| `DOCKERHUB_USERNAME` | **Yes** | You | Tagged releases fail fast, by design. |
| `DOCKERHUB_TOKEN` | **Yes** | You | Same. |
| `HOMEBREW_TAP_GITHUB_TOKEN` | Recommended | You | Release succeeds, but the Homebrew tap silently stays on the previous version. |
| `CODECOV_TOKEN` | Optional | You | Coverage upload is skipped. CI still passes. |

### `DOCKERHUB_USERNAME` and `DOCKERHUB_TOKEN`

The in-cluster server image is published to Docker Hub as
`docker.io/omrieival/ktunnel`. This is the registry `ktunnel expose`
pulls from by default, so it is not optional — a tagged release without
these credentials fails immediately with an explanatory error rather
than publishing binaries that point at an image nobody pushed.

Nothing has been pushed to Docker Hub since September 2023, so assume
the existing token has expired and generate a new one.

1. Sign in to [hub.docker.com](https://hub.docker.com) as the account
   that owns the `omrieival/ktunnel` repository.
2. Go to **Account settings → Personal access tokens →
   Generate new token**.
3. Description: `ktunnel GitHub Actions`.
4. Access permissions: **Read & Write**. (`Read, Write, Delete` also
   works but grants more than the workflow needs.)
5. Set an expiry you will actually notice — a year is reasonable. Note
   the date somewhere; an expired token is the most likely future cause
   of a failed release.
6. Copy the token. Docker Hub shows it exactly once.

Then add two repository secrets:

- `DOCKERHUB_USERNAME` — the Docker Hub **account name**, not the email
  address you sign in with. For this project that is `omrieival`.
- `DOCKERHUB_TOKEN` — the token from step 6.

> Images are also published to `ghcr.io/omrikiei/ktunnel` using the
> automatic `GITHUB_TOKEN`, which needs no setup. That mirror exists to
> sidestep Docker Hub's anonymous pull limits. It is not a substitute:
> the default `--server-image` still points at Docker Hub, so existing
> installs depend on the Docker Hub push succeeding.

### `HOMEBREW_TAP_GITHUB_TOKEN`

goreleaser updates the formula in the separate
[`omrikiei/homebrew-ktunnel`](https://github.com/omrikiei/homebrew-ktunnel)
repository. The workflow's automatic `GITHUB_TOKEN` is scoped to *this*
repository only and cannot write there, so a dedicated token is needed.

Without it the release still completes and the formula is still built
into `dist/`; it just never reaches the tap, and `brew install` keeps
serving the old version. That silence is why this is worth doing now —
it is exactly how the tap ended up stranded on 1.6.1.

Use a **fine-grained** personal access token, which can be scoped to the
one repository:

1. Go to
   [github.com/settings/personal-access-tokens/new](https://github.com/settings/personal-access-tokens/new).
2. Token name: `ktunnel-homebrew-tap`.
3. Expiration: your choice. Set a calendar reminder — goreleaser will
   fail loudly at renewal time, but only during a release.
4. Resource owner: `omrikiei`.
5. Repository access: **Only select repositories** → `omrikiei/homebrew-ktunnel`.
6. Permissions → Repository permissions → **Contents: Read and write**.
   Nothing else is needed.
7. Generate, then copy the token.

Add it as the repository secret `HOMEBREW_TAP_GITHUB_TOKEN`.

> A classic PAT with the `repo` scope also works, but it grants write
> access to every repository you own. Prefer the fine-grained token.

### `CODECOV_TOKEN` (optional)

`codecov/codecov-action@v4` wants a token. Coverage upload is configured
with `fail_ci_if_error: false`, so CI passes either way. If you want the
coverage badge to keep updating, grab the repository upload token from
[app.codecov.io](https://app.codecov.io) under the repo's settings and
add it as `CODECOV_TOKEN`.

## Cutting a release

### 1. Set the version

The version is stamped into the binary from the git tag via ldflags, so
there is nothing to edit. The literal in `cmd/root.go` is only used for
local `go build`.

This matters more than it looks: the default `--server-image` tag is
derived from the version string. Tagging `v2.1.0` makes every `ktunnel
expose` pull `docker.io/omrieival/ktunnel:v2.1.0`, which only exists
because the same workflow run pushed it.

### 2. Rehearse with a prerelease tag

Do this the first time after any change to the pipeline. It exercises
the entire path — goreleaser, the multi-arch image build, both
registries — without touching the Homebrew tap, because `prerelease:
auto` skips the tap update for tags that look like prereleases.

```sh
git tag v2.0.0-rc1
git push origin v2.0.0-rc1
```

Watch the run under **Actions**. Confirm all three of:

- the `goreleaser` job uploaded archives, `.deb` and `.rpm` files, and
  `checksums.txt` to the GitHub release
- the `image` job pushed to both `docker.io/omrieival/ktunnel` and
  `ghcr.io/omrikiei/ktunnel`
- the published binary reports the right version:

```sh
docker run --rm docker.io/omrieival/ktunnel:v2.0.0-rc1 version
```

If anything failed, delete the tag and the draft release, fix, repeat:

```sh
git push --delete origin v2.0.0-rc1
git tag -d v2.0.0-rc1
```

### 3. Tag the real release

```sh
git tag v2.0.0
git push origin v2.0.0
```

### 4. Verify

```sh
# the image the CLI will actually deploy
docker run --rm docker.io/omrieival/ktunnel:v2.0.0 version

# the tap picked up the new formula
brew update && brew info omrikiei/ktunnel/ktunnel
```

The krew index is updated by a separate bot
(`rajatjindal/krew-release-bot`), which opens a pull request against
`kubernetes-sigs/krew-index`. That lands on their schedule, not yours.

## How the pipeline is wired

Understanding this makes failures much faster to diagnose.

- **`test.yml`** runs on every push to master and every pull request:
  formatting, `go vet`, `go test -race`, and `gosec`.
- **`release.yaml`** has two jobs.
  - `goreleaser` is gated on `startsWith(github.ref, 'refs/tags/v')`. It
    only ever runs for tags.
  - `image` runs on **tags, master, and pull requests**. On pull
    requests it builds without pushing. This is deliberate: the image
    job used to run only on tags, which meant a broken Dockerfile was
    undiscoverable until after a release was already public.

Two pins are load-bearing and should be changed on purpose, never by
drift:

- **goreleaser is pinned to `~> v2.18`.** This config previously sat on
  `latest` and rotted into a state goreleaser v2 refused to parse, which
  would have failed the first tagged release outright.
- **The Go version in both workflows and the Dockerfile must match the
  `go` directive in `go.mod`.** When they drifted apart, the image could
  not be built at all, and CI failed with the memorable
  `go: no such tool "covdata"`.

The archive name template in `.goreleaser.yml` is also load-bearing:
`.krew.yaml` and `ktunnel.rb` both construct download URLs from it.
Changing it breaks krew and Homebrew installs.

## Known deprecation

`brews` is deprecated in goreleaser in favour of `homebrew_casks`. It
still works on the pinned version. Migrating changes the install command
for users and needs a `tap_migrations.json` in the tap repository, so it
should be its own change with its own release note rather than a
side-effect of a routine release.
