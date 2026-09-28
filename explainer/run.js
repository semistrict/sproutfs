// Runs the real test binary, compiled to WebAssembly, off the page's thread.
// The page terminates this worker once the binary exits, which frees the Go
// heap; the page keeps only the chapters it printed.
importScripts("wasm_exec.js");

// The Go runtime writes the test binary's stdout through globalThis.fs; each
// chapter prints one "EXPLAIN {json}" line, posted to the page as it arrives.
let pending = "";
const decoder = new TextDecoder();
const writeSync = globalThis.fs.writeSync.bind(globalThis.fs);
globalThis.fs.writeSync = (fd, buffer) => {
  if (fd === 1) {
    pending += decoder.decode(buffer);
    let newline;
    while ((newline = pending.indexOf("\n")) >= 0) {
      const line = pending.slice(0, newline);
      pending = pending.slice(newline + 1);
      if (line.startsWith("EXPLAIN ")) postMessage({ run: JSON.parse(line.slice(8)) });
    }
    return buffer.length;
  }
  return writeSync(fd, buffer);
};
// The glue answers a write's callback before it returns, which re-enters Go on
// the writing goroutine, inside the simulation's synctest bubble, and panics.
// Answer on a microtask, as Node answers on a later tick.
globalThis.fs.write = (fd, buffer, offset, length, position, callback) => {
  if (offset !== 0 || length !== buffer.length || position !== null) {
    queueMicrotask(() => callback(new Error("the explainer writes whole buffers only")));
    return;
  }
  const written = globalThis.fs.writeSync(fd, buffer);
  queueMicrotask(() => callback(null, written));
};

(async () => {
  const go = new Go();
  go.argv = ["simtest.wasm", "-test.run=^TestExplain"];
  // The chapters are small; a tight limit keeps the heap, which WebAssembly
  // memory never gives back, near what they use.
  go.env = { SPROUTFS_EXPLAIN: "1", GOMEMLIMIT: "96MiB" };
  const { instance } = await WebAssembly.instantiateStreaming(fetch("simtest.wasm"), go.importObject);
  go.exit = (code) => {
    // Timers and write callbacks the binary left behind have nothing to resume.
    go._resume = () => {};
    postMessage({ exit: code });
  };
  go.run(instance);
})().catch((error) => postMessage({ error: String(error) }));
