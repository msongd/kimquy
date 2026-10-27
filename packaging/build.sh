#!/bin/bash
# build.sh builds "Kim Quy.app" and KimQuy-<version>.dmg into $OUT.
#
#   packaging/build.sh
#
# Environment:
#   VERSION         app version (default 0.1.0)
#   OUT             output directory (default packaging/out)
#   BUNDLE_MODELS   0 (default): a slim app without the model, which it downloads at
#                   first launch (about 50 MB instead of 1.1 GB); 1: bundle MODELS in it
#   MODELS          with BUNDLE_MODELS=1: the model directory, holding text-q8 and any of
#                   vision-q8, audio-bf16 (default models/release)
#   SIGN_ID         codesign identity, e.g. "Developer ID Application: … (TEAMID)";
#                   default "-" signs ad hoc (runs on this Mac only, Gatekeeper rejects it elsewhere)
#   NOTARY_PROFILE  notarytool keychain profile (xcrun notarytool store-credentials);
#                   when set, the dmg is notarized and stapled (needs a real SIGN_ID)
#   NO_DMG          1: stop after the signed app (no dmg, no notarization)
set -euo pipefail

die() { echo "build.sh: $*" >&2; exit 1; }
step() { echo "==> $*" >&2; }

REPO=$(cd "$(dirname "$0")/.." && pwd)
VERSION=${VERSION:-0.1.0}
OUT=${OUT:-$REPO/packaging/out}
BUNDLE_MODELS=${BUNDLE_MODELS:-0}
MODELS=${MODELS:-$REPO/models/release}
SIGN_ID=${SIGN_ID:--}
NOTARY_PROFILE=${NOTARY_PROFILE:-}
NO_DMG=${NO_DMG:-0}
NAME="Kim Quy" # the app as people see it
EXE=KimQuy     # its executable and dmg name: no space
BUNDLE_ID=dev.kimquy.app # = agent.BundleID
MIN_MACOS=14.0

[[ $(uname -s) == Darwin && $(uname -m) == arm64 ]] || die "needs macOS on Apple Silicon"
[[ $VERSION =~ ^[0-9]+(\.[0-9]+){1,2}$ ]] || die "VERSION $VERSION is not N.N[.N]"
grep -q "BundleID *= \"$BUNDLE_ID\"" "$REPO/internal/agent/agent.go" || die "BUNDLE_ID differs from agent.BundleID"
[[ $BUNDLE_MODELS == 0 || $BUNDLE_MODELS == 1 ]] || die "BUNDLE_MODELS must be 0 or 1, not $BUNDLE_MODELS"
if [[ $BUNDLE_MODELS == 1 ]]; then
	# semd-embed takes a part to be there when its manifest is.
	[[ -f $MODELS/text-q8/manifest.json ]] || die "missing model $MODELS/text-q8"
	for m in "$MODELS"/*; do
		case $(basename "$m") in
		text-q8 | vision-q8 | audio-bf16) ;;
		*) die "unexpected $m in MODELS: only text-q8, vision-q8, audio-bf16 belong" ;;
		esac
	done
fi
[[ $NO_DMG == 0 || $NO_DMG == 1 ]] || die "NO_DMG must be 0 or 1, not $NO_DMG"
[[ $NO_DMG == 0 || -z $NOTARY_PROFILE ]] || die "NOTARY_PROFILE needs the dmg; unset NO_DMG"
if [[ -n $NOTARY_PROFILE && $SIGN_ID == - ]]; then
	die "NOTARY_PROFILE needs a Developer ID SIGN_ID; ad hoc signatures cannot be notarized"
fi
for tool in go xcodebuild codesign diskutil hdiutil plutil ditto; do
	command -v "$tool" >/dev/null || die "$tool not found"
done

BUILD=$OUT/build
APP="$OUT/$NAME.app"
DMG="$OUT/$EXE-$VERSION.dmg"
rm -rf "$BUILD" "$APP" "$DMG"
mkdir -p "$BUILD"

step "semd-embed (Swift, MLX)"
PRODUCTS=$REPO/embedder/.build-xcode/Build/Products/Release
(cd "$REPO/embedder" && xcodebuild -quiet -scheme semd-embed -configuration Release \
	-destination 'platform=macOS,arch=arm64' -derivedDataPath .build-xcode \
	CLANG_ENABLE_CODE_COVERAGE=NO CLANG_COVERAGE_MAPPING=NO SWIFT_ENABLE_CODE_COVERAGE=NO build) >&2
[[ -f $PRODUCTS/mlx-swift_Cmlx.bundle/Contents/Resources/default.metallib ]] || die "MLX metallib was not built"

step "semd, semctl (Go, cgo for FSEvents)"
export GOOS=darwin GOARCH=arm64 GOFLAGS=-trimpath MACOSX_DEPLOYMENT_TARGET=$MIN_MACOS
# Go's build cache ignores MACOSX_DEPLOYMENT_TARGET; without these it reuses
# cgo objects built for the host's macOS.
export CGO_CFLAGS="-O2 -g -mmacosx-version-min=$MIN_MACOS" CGO_LDFLAGS="-mmacosx-version-min=$MIN_MACOS"
(cd "$REPO" && CGO_ENABLED=1 go build -ldflags=-s -o "$BUILD/semd" ./cmd/semd)
(cd "$REPO" && CGO_ENABLED=1 go build -ldflags=-s -o "$BUILD/semctl" ./cmd/semctl)
step "$EXE (Go, MyGo, no cgo)"
(cd "$REPO/ui" && CGO_ENABLED=0 go build -ldflags=-s -o "$BUILD/$EXE" .)
step "AppIcon.icns (drawn by packaging/icon)"
(cd "$REPO" && go run ./packaging/icon -out "$BUILD/AppIcon.iconset")
iconutil -c icns -o "$BUILD/AppIcon.icns" "$BUILD/AppIcon.iconset"
[[ -s $BUILD/AppIcon.icns ]] || die "iconutil made no AppIcon.icns"

step "assemble $APP"
C=$APP/Contents
mkdir -p "$C/MacOS" "$C/Helpers" "$C/Resources"
install -m 755 "$BUILD/$EXE" "$C/MacOS/$EXE"
install -m 755 "$BUILD/semd" "$BUILD/semctl" "$PRODUCTS/semd-embed" "$C/Helpers/"
install -m 644 "$BUILD/AppIcon.icns" "$C/Resources/AppIcon.icns"
# SwiftPM resource bundles (the MLX metallib among them) are looked up next
# to the executable that uses them.
BUNDLES=(mlx-swift_Cmlx.bundle swift-crypto_Crypto.bundle swift-transformers_Hub.bundle)
for b in "${BUNDLES[@]}"; do
	ditto "$PRODUCTS/$b" "$C/Helpers/$b"
done
if [[ $BUNDLE_MODELS == 1 ]]; then
	ditto "$MODELS" "$C/Resources/models"
fi
cat >"$C/Info.plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleName</key><string>$NAME</string>
	<key>CFBundleDisplayName</key><string>$NAME</string>
	<key>CFBundleIdentifier</key><string>$BUNDLE_ID</string>
	<key>CFBundleVersion</key><string>$VERSION</string>
	<key>CFBundleShortVersionString</key><string>$VERSION</string>
	<key>CFBundleExecutable</key><string>$EXE</string>
	<key>CFBundlePackageType</key><string>APPL</string>
	<key>CFBundleIconFile</key><string>AppIcon</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleDevelopmentRegion</key><string>en</string>
	<key>LSMinimumSystemVersion</key><string>$MIN_MACOS</string>
	<key>LSArchitecturePriority</key><array><string>arm64</string></array>
	<key>LSApplicationCategoryType</key><string>public.app-category.productivity</string>
	<key>NSPrincipalClass</key><string>NSApplication</string>
	<key>NSHighResolutionCapable</key><true/>
	<key>NSSupportsAutomaticGraphicsSwitching</key><true/>
</dict>
</plist>
EOF
plutil -lint -s "$C/Info.plist" || die "bad Info.plist"
printf 'APPL????' >"$C/PkgInfo"

step "sign ($([[ $SIGN_ID == - ]] && echo "ad hoc" || echo "$SIGN_ID"))"
SIGN=(codesign --force --options runtime --sign "$SIGN_ID")
if [[ $SIGN_ID == - ]]; then
	SIGN+=(--timestamp=none)
else
	SIGN+=(--timestamp)
fi
# Inside out: nested code first, the app last.
for b in "${BUNDLES[@]}"; do
	"${SIGN[@]}" "$C/Helpers/$b"
done
"${SIGN[@]}" --identifier dev.kimquy.semd-embed "$C/Helpers/semd-embed"
"${SIGN[@]}" --identifier dev.kimquy.semd "$C/Helpers/semd"
"${SIGN[@]}" --identifier dev.kimquy.semctl "$C/Helpers/semctl"
"${SIGN[@]}" "$APP"
codesign --verify --strict --deep "$APP" || die "signature does not verify"

if [[ $NO_DMG == 1 ]]; then
	rm -rf "$BUILD"
	step "done: $APP ($(du -sh "$APP" | cut -f1)$([[ $BUNDLE_MODELS == 1 ]] && echo ", model bundled")), no dmg (NO_DMG=1)"
	exit 0
fi

step "dmg"
STAGE=$BUILD/dmg
mkdir -p "$STAGE"
ditto "$APP" "$STAGE/$NAME.app"
ln -s /Applications "$STAGE/Applications"
# hdiutil create is deprecated from macOS 26 in favour of diskutil image.
if diskutil image create from -h >/dev/null 2>&1; then
	diskutil image create from --format ULFO --volumeName "$NAME" "$STAGE" "$DMG" >/dev/null
	diskutil image info "$DMG" >/dev/null || die "dmg is unreadable"
else
	hdiutil create -quiet -volname "$NAME" -srcfolder "$STAGE" -format ULFO "$DMG"
	hdiutil verify -quiet "$DMG" || die "dmg does not verify"
fi
if [[ $SIGN_ID != - ]]; then
	codesign --force --sign "$SIGN_ID" --timestamp "$DMG"
fi

if [[ -n $NOTARY_PROFILE ]]; then
	step "notarize"
	xcrun notarytool submit "$DMG" --keychain-profile "$NOTARY_PROFILE" --wait
	xcrun stapler staple "$DMG"
	spctl --assess --type open --context context:primary-signature -v "$DMG"
else
	echo "build.sh: WARNING: not notarized (NOTARY_PROFILE unset): Gatekeeper on other Macs will block this build" >&2
fi
rm -rf "$BUILD"
step "done: $APP, $DMG ($(du -sh "$DMG" | cut -f1)$([[ $BUNDLE_MODELS == 1 ]] && echo ", model bundled"))"
