//! The summary plugin in Zig: the same input, output and errors as ../summary (Go), a plain program too.
//!
//!   stdin:  {"title": "...", "body": "..."}
//!   stdout: {"text": "<title>: <first 12 words> …", "words": <word count>}
//!
//! Build: zig build -Doptimize=ReleaseSmall (Zig 0.14+); the module is zig-out/bin/summary.wasm.
const std = @import("std");

const Post = struct { title: []const u8 = "", body: []const u8 = "" };

/// Ends the program with an error pb_hooks can read (err.code / err.message).
fn fail(code: []const u8, message: []const u8) noreturn {
    std.io.getStdErr().writer().print("{{\"code\":\"{s}\",\"message\":\"{s}\"}}", .{ code, message }) catch {};
    std.process.exit(1);
}

pub fn main() !void {
    var arena = std.heap.ArenaAllocator.init(std.heap.wasm_allocator);
    defer arena.deinit();
    const allocator = arena.allocator();

    const input = try std.io.getStdIn().readToEndAlloc(allocator, 16 << 20);
    const post = std.json.parseFromSliceLeaky(Post, allocator, input, .{ .ignore_unknown_fields = true }) catch
        fail("bad_input", "the input isn't a post");

    var words = std.ArrayList([]const u8).init(allocator);
    var it = std.mem.tokenizeAny(u8, post.body, " \t\r\n");
    while (it.next()) |w| try words.append(w);
    if (words.items.len == 0) fail("empty_body", "the post has no body to summarize");

    var text = std.ArrayList(u8).init(allocator);
    if (post.title.len > 0) try text.writer().print("{s}: ", .{post.title});
    for (words.items[0..@min(words.items.len, 12)], 0..) |w, i| {
        if (i > 0) try text.append(' ');
        try text.appendSlice(w);
    }
    if (words.items.len > 12) try text.appendSlice(" …");

    try std.json.stringify(.{ .text = text.items, .words = words.items.len }, .{}, std.io.getStdOut().writer());
}
