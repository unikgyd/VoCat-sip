#!/bin/sh
set -eu

# Local diagnostics must not start pcscd or touch hardware, even through the
# standard container entrypoint. They also work in a read-only sandbox.
case "${1:-}" in
  version|-v|--version|help|-h|--help|media-check)
    exec /opt/vocat/bin/vocat "$@"
    ;;
esac

# pcscd daemonizes after startup. Keep failure non-fatal so modem-only
# deployments remain usable and the UI can report a reader diagnostic.
if [ "$(id -u)" = "0" ] && command -v pcscd >/dev/null 2>&1; then
  mkdir -p /run/pcscd
  pcscd || echo "warning: pcscd failed to start; USB SIM readers may be unavailable" >&2
fi

exec /opt/vocat/bin/vocat "$@"
