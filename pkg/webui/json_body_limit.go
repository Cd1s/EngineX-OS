package webui

// jsonBodyLimit bounds a small JSON request body (settings, a hot cue, a mount
// id). The decoder reads a value to its end before it returns, so without a
// bound one request could make stream-sync hold a body of any size in memory
// (BH2-A5-14); the other JSON handlers already had their own limit.
const jsonBodyLimit = 64 << 10
