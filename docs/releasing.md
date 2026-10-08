# Releasing

A release is a pushed tag `vX.Y.Z` or `vX.Y.Z-rcN` on a commit of
`develop` whose push run of `ci.yml` passed. `release.yml` checks that
again (`make release-gate`), runs `make vuln`, and publishes the release at
once: deb, rpm and apk packages, the binaries, `checksums.txt` and a build
provenance attestation of every file in it. A tag `-rcN` becomes a
prerelease; the number has no dot before it, because the apk version of
`-rc.1` would be `_rc.1`, which apk does not accept, and `release-gate`
refuses any other suffix. `main` takes every release by fast-forward,
an rc included, because the gate wants the tagged commit in `main`; after a
failed release `main` stays on its commit until the next release.

The release notes list the commits since the last final tag before the
tagged commit, so an rc and the final release on its commit get the same
notes; before the first final release they start at `CHANGELOG_START` of
the `Makefile`.

The tag ruleset forbids deleting a `v*` tag and updating it other than by
fast-forward. A pushed tag is never moved at all; a mistake is fixed by the
next version number.

## Procedure

From a clone with push access, `gh` logged in to the repository.

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

   A final version, one without a suffix, also needs a run on Rocky 9 and
   Rocky 10 recorded under "Result" of [selinux.md](selinux.md); no job
   checks that.

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
     (`-rcN+1`, or the next patch version). The failed tag stays.
