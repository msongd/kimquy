// swift-tools-version: 6.3
// semd-embed: the embedding sidecar for semd. Build with Xcode (xcodebuild)
// so the MLX Metal shaders are compiled and bundled:
//
//   xcodebuild -scheme semd-embed -configuration Release \
//     -destination 'platform=macOS,arch=arm64' -derivedDataPath .build-xcode \
//     CLANG_ENABLE_CODE_COVERAGE=NO CLANG_COVERAGE_MAPPING=NO SWIFT_ENABLE_CODE_COVERAGE=NO build
//
// Without the coverage overrides the generated scheme instruments Release too.
import PackageDescription

let package = Package(
    name: "semd-embed",
    platforms: [.macOS(.v14)],
    products: [
        .executable(name: "semd-embed", targets: ["semd-embed"])
    ],
    dependencies: [
        // Exact pins, matching the vendored port's validated versions.
        .package(url: "https://github.com/ml-explore/mlx-swift", exact: "0.32.3"),
        .package(url: "https://github.com/huggingface/swift-transformers", exact: "1.3.0"),
    ],
    targets: [
        .target(
            name: "EmbeddingGemma2",
            dependencies: [
                .product(name: "MLX", package: "mlx-swift"),
                .product(name: "MLXNN", package: "mlx-swift"),
                .product(name: "Tokenizers", package: "swift-transformers"),
            ],
            path: "Vendor/EmbeddingGemma2Swift/EmbeddingGemma2"),
        .executableTarget(
            name: "semd-embed",
            dependencies: ["EmbeddingGemma2", .product(name: "MLX", package: "mlx-swift")],
            path: "Sources/semd-embed"),
        .testTarget(
            name: "EmbeddingGemma2Tests",
            dependencies: ["EmbeddingGemma2"],
            path: "Vendor/EmbeddingGemma2Swift/Tests/EmbeddingGemma2Tests"),
    ])
