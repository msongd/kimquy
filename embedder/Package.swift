// swift-tools-version: 6.3
// semd-embed: the embedding sidecar for semd, running EmbeddingGemma 2
// through the LiteRT-LM C API.
//
//   swift build -c release --arch arm64
//
// The executable loads @rpath/libCLiteRTLM_mac.dylib: next to it in a
// build (@loader_path), from Contents/Frameworks in the app.
import PackageDescription

let package = Package(
    name: "semd-embed",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "semd-embed", targets: ["semd-embed"])
    ],
    targets: [
        // LiteRT-LM v0.19.0 (Apache-2.0), pinned by the archive's sha256.
        .binaryTarget(
            name: "CLiteRTLM",
            url: "https://github.com/google-ai-edge/LiteRT-LM/releases/download/v0.19.0/CLiteRTLM_mac.xcframework.zip",
            checksum: "1a9e283a99a119aefe21afda541a5308a7d88aa9cb12f58aa7974038e3369c7c"),
        .executableTarget(
            name: "semd-embed",
            dependencies: ["CLiteRTLM"],
            path: "Sources/semd-embed",
            linkerSettings: [
                .unsafeFlags([
                    "-Xlinker", "-rpath", "-Xlinker", "@loader_path",
                    "-Xlinker", "-rpath", "-Xlinker", "@executable_path/../Frameworks",
                ])
            ]),
    ])
