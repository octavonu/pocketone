// ---- ext/wasm: $wasm ----
// The pb_hooks API of ext/wasm. The JS engine adds it to pb_data/types.d.ts on every app ext/wasm runs on. ext/wasm is
// an extension; the files it runs, pb_hooks/<name>.pb.wasm, are plugins.

/**
 * What a plugin gets on stdin: a string or bytes (a typed array or an ArrayBuffer) as they are, nothing for undefined or null, anything else as JSON.
 */
type WasmInput = string | ArrayBuffer | Uint8Array | Array<number> | object | number | boolean | null | undefined

/** One call to try a plugin with, from its manifest (what it prints for `<name> --describe`). */
interface WasmExample {
  /** "run" when the manifest leaves it out. */
  function: string
  /** A string is the plugin's stdin as it is (EDIFACT, XML, CSV); anything else is sent as JSON. */
  input: any
  /** What it gives, when the example says. */
  output?: any
}

/** How one plugin in pb_hooks/ is doing, with what it says for --describe. */
interface WasmPluginInfo {
  name: string
  /** Its module checks out: it can be called. */
  running: boolean
  description?: string
  /** The author's version, as the manifest writes it. */
  version?: string
  /** The functions it says it has; ["run"] by default. */
  functions?: Array<string>
  /** What it's written in: what the module says, else what its manifest says. */
  language?: string
  /** Where its source is: an http(s) URL. */
  repository?: string
  examples?: Array<WasmExample>
  /** Its module's size in bytes: what a call of it takes of the app's `maxModulesMB` while it runs. */
  size?: number
  /** Why it can't run (running false), or what's wrong with what it printed for --describe. */
  error?: string
}

/** What every plugin call in the app runs within. */
interface WasmLimits {
  /** Memory each call may allocate (heap, stack, input). Every call has its own, not shared. */
  maxCallMemoryMB: number
  /** Time limit for a call's run (compiling comes before it). */
  timeoutMs: number
  /** Calls running at once. */
  maxConcurrent: number
  /**
   * MB of modules running at once: the sizes of the calls' modules, added up. With 10, two calls of a Go plugin
   * (~4.4 MB) run at once, and `maxConcurrent` of a Rust or Zig one (~0.1 MB). Also the largest module that loads.
   */
  maxModulesMB: number
  /** Largest input or output of a call, in bytes. */
  maxPayload: number
}

/** The codes ext/wasm gives; a plugin can give its own (it exits non-zero with {"code", "message"} on stderr). */
type WasmErrorCode =
  | "not_found" // no <name>.pb.wasm in pb_hooks/
  | "not_loaded" // in the folder, but it can't run (the message says why)
  | "no_such_function" // the plugin has no such function (the code plugins give by convention)
  | "timeout" // over the time limit
  | "crashed" // the plugin trapped, panicked or ran out of memory
  | "too_large" // input or output over the size limit
  | "busy" // the calls running left no room for this one for too long (maxConcurrent, maxModulesMB)
  | "plugin_error" // the plugin exited non-zero with no code of its own
  | (string & {})

/** What a failed $wasm.run or $wasm.call throws: catch it to decide what the hook does next. */
interface WasmError extends Error {
  name: "WasmError"
  /** The plugin's name. */
  plugin: string
  code: WasmErrorCode
  /** Whether the same call may work later (busy, timeout...). */
  retryable: boolean
}

/** What $wasm.inspect gives: one call, with its log and its timings. */
interface WasmInspection<T = any> {
  /** The output; null when the call failed or the plugin wrote nothing. */
  output: T | null
  /** What the plugin wrote to stderr: its log. */
  stderr: string
  /** Why the call failed, or null. */
  error: { code: WasmErrorCode; message: string; retryable: boolean } | null
  compileMs: number
  runMs: number
}

/**
 * The app's plugins, the pb_hooks/<name>.pb.wasm files. A plugin is a closed box: it gets its input and gives its output,
 * and reaches no database, file, network, environment or process. The call blocks the hook while the plugin runs.
 *
 * The output is the plugin's stdout: JSON parsed, a string when it isn't JSON, null when it's empty.
 */
declare namespace $wasm {
  /**
   * Calls the plugin's "run". Throws a {@link WasmError} when the call fails.
   *
   * @example
   * const out = $wasm.run("summary", { title: e.record.get("title"), body: e.record.get("body") })
   */
  function run<T = any>(name: string, input?: WasmInput): T
  namespace run {
    /**
     * {@link run}, off the app's JS VM: the app serves other requests while the plugin runs. Use it in an async hook
     * for a slow plugin (Go) or many calls at once.
     *
     * @example
     * onRecordCreate(async (e) => {
     *   const out = await $wasm.run.async("summary", { title: e.record.get("title"), body: e.record.get("body") })
     *   e.record.set("summary", out.text)
     *   return e.next()
     * }, "posts")
     */
    function async<T = any>(name: string, input?: WasmInput): Promise<T>
  }

  /**
   * Calls one of the plugin's functions (its first argument). Throws a {@link WasmError} when the call fails.
   *
   * @example
   * const r = $wasm.call("crypto", "decrypt", { key, data })
   */
  function call<T = any>(name: string, fn: string, input?: WasmInput): T
  namespace call {
    /** {@link call}, off the app's JS VM, like {@link run.async}. */
    function async<T = any>(name: string, fn: string, input?: WasmInput): Promise<T>
  }

  /** Every plugin in pb_hooks/ and how it's doing. */
  function plugins(): Array<WasmPluginInfo>

  /** What every call runs within: memory, time, how many calls and how many MB of modules at once. */
  function limits(): WasmLimits

  /** Calls a function to try a plugin out: never throws for the call, and gives its log and timings. */
  function inspect<T = any>(name: string, fn: string, input?: WasmInput): WasmInspection<T>
  namespace inspect {
    /** {@link inspect}, off the app's JS VM, like {@link run.async}. */
    function async<T = any>(name: string, fn: string, input?: WasmInput): Promise<WasmInspection<T>>
  }
}
