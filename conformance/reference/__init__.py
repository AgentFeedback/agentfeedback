"""Reference implementation of the AgentFeedback v1 write path, used to check
the conformance fixtures. Standard library only. Not a client and not a
server: it exists so that the fixtures are verified by an implementation that
shares no code with the Go service."""

import sys as _sys

# A body may nest 512 levels (rawjson.MAX_DEPTH); the reader and the canonical
# writer recurse a few frames per level, well past Python's default limit.
_sys.setrecursionlimit(max(_sys.getrecursionlimit(), 20_000))
