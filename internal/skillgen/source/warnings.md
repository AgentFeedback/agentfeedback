## Warnings are advice

A response can carry `warnings`, each with a `code`, a `pointer` to the
member and a `message`. The report was stored anyway; warnings say what would
make the next one better (a missing recommended member, a value that was
truncated or moved). Do not resubmit because of a warning, and mention it to
the user only if it points at a mistake in what you sent.
<!-- only: cli -->

The binary relays the server's warnings on stderr, one per line.
<!-- end -->
