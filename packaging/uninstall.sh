#!/bin/bash
# uninstall.sh stops semd and removes its LaunchAgent. With --purge it also
# deletes the index, settings and logs, and the app's preferences, caches
# and macOS permissions, as Settings › Service › Uninstall in the app does.
# The app itself goes to the Trash by hand (it is wherever the user put it).
#
#   packaging/uninstall.sh [--purge]
set -euo pipefail

LABEL=dev.kimquy.semd # = agent.DefaultLabel
BUNDLE_ID=dev.kimquy.app # = agent.BundleID
STATE="$HOME/Library/Application Support/Kim Quy"
LOGS="$HOME/Library/Logs/Kim Quy"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

purge=false
case "${1:-}" in
"") ;;
--purge) purge=true ;;
*) echo "usage: $0 [--purge]" >&2; exit 2 ;;
esac

if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then
	launchctl bootout "gui/$(id -u)/$LABEL"
	for _ in $(seq 120); do
		launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1 || break
		sleep 0.25
	done
	if launchctl print "gui/$(id -u)/$LABEL" >/dev/null 2>&1; then
		echo "uninstall.sh: $LABEL is still loaded" >&2
		exit 1
	fi
	echo "stopped $LABEL"
fi
rm -f "$PLIST"
echo "removed $PLIST"

if $purge; then
	# Refuse while a semd (one started by hand: the agent is gone) serves it.
	if [[ -e $STATE/semd.sock ]] && lsof -t "$STATE/semd.sock" >/dev/null 2>&1; then
		echo "uninstall.sh: a semd is still serving $STATE (started by hand?); stop it first" >&2
		exit 1
	fi
	rm -rf "$STATE" "$LOGS"
	echo "removed $STATE and $LOGS"

	# What macOS keeps for the app, by its bundle ID (see ui/uninstall.go).
	defaults delete "$BUNDLE_ID" >/dev/null 2>&1 || true
	lib="$HOME/Library"
	cache=$(getconf DARWIN_USER_CACHE_DIR)
	[[ $cache == /* ]] || { echo "uninstall.sh: bad DARWIN_USER_CACHE_DIR: $cache" >&2; exit 1; }
	for p in "$lib/Preferences/$BUNDLE_ID.plist" "$lib/Caches/$BUNDLE_ID" \
		"$lib/Saved Application State/$BUNDLE_ID.savedState" "$lib/HTTPStorages/$BUNDLE_ID" \
		"$lib/HTTPStorages/$BUNDLE_ID.binarycookies" "$lib/WebKit/$BUNDLE_ID" "${cache%/}/$BUNDLE_ID"; do
		if [[ -e $p ]]; then
			rm -rf -- "$p"
			echo "removed $p"
		fi
	done
	# Exit 64 when macOS does not know the app (it is already gone).
	tccutil reset All "$BUNDLE_ID" >/dev/null 2>&1 || true
	echo "reset the macOS permissions of $BUNDLE_ID"
fi
echo "Quit Kim Quy, remove it from Login Items, and drag it to the Trash to finish. Remove semd from Full Disk Access in System Settings if you added it."
