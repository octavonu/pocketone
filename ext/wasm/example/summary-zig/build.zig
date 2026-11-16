const std = @import("std");

// zig build -Doptimize=ReleaseSmall  ->  zig-out/bin/summary.wasm (a WASI program)
pub fn build(b: *std.Build) void {
    const optimize = b.standardOptimizeOption(.{});
    const target = b.standardTargetOptions(.{
        .default_target = .{ .os_tag = .wasi, .cpu_arch = .wasm32 },
    });
    const plugin = b.addExecutable(.{
        .name = "summary",
        .root_source_file = b.path("src/main.zig"),
        .target = target,
        .optimize = optimize,
    });
    // Zig's default stack is 16 MB: every call would start with 16 MB of memory. 1 MB is plenty here.
    plugin.stack_size = 1 << 20;
    b.installArtifact(plugin);
}
