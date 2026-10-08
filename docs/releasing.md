# Releasing

A release is a pushed tag `vX.Y.Z` or `vX.Y.Z-rcN` on a commit of
`develop` whose push run of `ci.yml` passed. `release.yml` checks that
again (`make release-gate`), runs `make vuln`, and publishes the release at
once: deb, rpm and apk packages, the binaries, `checksums.txt` and a build
provenance attestation of every file in it. A tag `-rcN` becomes a
prerelease; the number has no dot before it, because the apk version of
`-rc.1` would be `_rc.1`, which apk installs but cannot order against
another version, and no number has a leading zero, because `rc01` and `rc1`
would give packages of the same version; `release-gate` refuses any other
form. `main` takes every release by fast-forward, an rc included, because
the gate wants the tagged commit in `main`; after a failed release `main`
stays on its commit until the next release.

The release notes list the commits since the last final tag before the
tagged commit, so an rc and the final release on its commit get the same
notes; before the first final release they start at `CHANGELOG_START` of
the `Makefile`.

The tag ruleset forbids deleting a `v*` tag and updating it other than by
fast-forward. A pushed tag is never moved at all; a mistake is fixed by the
next version number.

## Procedure

From a clone with push access, `gh` logged in to the repository. A final
version, one without a suffix, first needs a run on Rocky 9 and Rocky 10
recorded under "Result" of [selinux.md](selinux.md) for an rpm built from
a commit whose `packaging`, `cmd` and `internal` match the one to release
(`git diff --quiet <commit of the run> origin/develop -- packaging cmd
internal` exits 0); no job checks that, and a record that is missing or
older stops the release before step 1, since a new record is a commit on
`develop`.

1. Fast-forward `main` to the head of `develop`, once the last push run of
   `ci.yml` for that commit is completed and green:

   ```sh
   git fetch origin
   sha=$(git rev-parse origin/develop)
   gh run list --workflow ci.yml --branch develop --event push --commit "$sha"
   git push origin "$sha:refs/heads/main"
   ```

   The push never takes `--force`. When the server refuses it, `main` has a
   commit `develop` lacks: stop the release until that is sorted out.

2. Check what cannot be undone. All of it must hold: the run of step 1 is
   green, the commit is in `main` (exit status 0), the tag is free (exit
   status 2) and so is the release (`release not found`):

   ```sh
   git fetch origin && git merge-base --is-ancestor "$sha" origin/main
   git ls-remote --exit-code --tags origin refs/tags/vX.Y.Z
   gh release view vX.Y.Z
   ```

3. Tag the commit and push the tag:

   ```sh
   git tag -a vX.Y.Z -m vX.Y.Z "$sha"
   git push origin vX.Y.Z
   ```

   `release.yml` starts on the tag; its run is listed by
   `gh run list --workflow release.yml`.

4. When the run fails:

   - `make release-gate` failed because the CI of the commit was still
     running or a job of it was rerun later: once that run is green,
     "Re-run failed jobs" of the `release.yml` run.
   - `make release` or the attestation failed on something outside the code
     (a network error, a GitHub outage): "Re-run failed jobs" as well. The
     rerun builds the same files again and replaces those a published
     release has. goreleaser uploads into a draft and publishes it last, so
     a failure during the upload leaves a draft behind that the rerun does
     not find: it creates a new one. Delete the old draft on the releases
     page; deleting a draft keeps the tag. With immutable releases turned on
     in the repository settings goreleaser refuses to change a published
     release, and such a rerun fails: the next version number then.
   - The code or the configuration has to change: a new commit on
     `develop`, its CI green, then steps 1 to 3 with the next number
     (`-rcN+1`, or the next patch version). The failed tag stays, and
     that new commit adds it to `unpublishedTags` of
     `smoke_previous_test.go`: otherwise `TestSmokePrevious` fails
     `make check` on every commit after the tag.

5. Once the release is published, point the upgrade smoke tests at it in a
   commit on `develop`: `SMOKE_PREVIOUS` of the `Makefile` names the newest
   final release, and before the first one the newest rc;
   `SMOKE_PREVIOUS_SUMS` is the sha256 of its `checksums.txt`:

   ```sh
   base=https://github.com/6RUN0/mailcrier/releases/download
   curl -fsSL "$base/vX.Y.Z/checksums.txt" | sha256sum
   ```

   Until that commit `TestSmokePrevious` fails `make check` on every commit
   after the tagged one; the tagged commit itself stays green. The same
   commit drops the entries of `unorderablePrevious` in
   `packaging/smoke/upgrade_test.go` that name the versions of the old pin;
   an entry for another version, or for one dpkg or apk can order, fails
   `TestSmokeUpgrade`.

   The release `SMOKE_PREVIOUS` names is never deleted, and its files are
   never replaced by a rerun: every smoke job fetches them and checks them
   against `SMOKE_PREVIOUS_SUMS`, so a missing or changed file fails all of
   them. A pinned release that has to be built again gets its new sum in the
   same push.
