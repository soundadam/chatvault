# What CI runs, in one command.
ci: verify-mod check test-race build

# The release archives for every platform, as the release workflow builds them.
package:
	@test -n "$(VERSION)" || (echo "VERSION is required, e.g. make package VERSION=0.1.0" >&2; exit 1)
	./scripts/package.sh "$(VERSION)"
