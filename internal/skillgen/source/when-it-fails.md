## When something fails

<!-- only: cli -->
Run `agentfeedback doctor` first. It checks the configuration, the
connection, the key, the server's version and the local spool, and says what
to do next. A machine without configuration needs
`agentfeedback doctor --init --url <server URL> --key-from-stdin` with the key
on stdin.
<!-- end -->
<!-- only: http -->
Call `GET {{server}}/api/v1/meta` with the same headers: `200` means the URL
and the key work, `401` means the key is wrong, `429` means wait and retry,
and no answer means the URL is wrong or the service is down.
<!-- end -->
<!-- only: mcp -->
A tool error that says `unauthorized` means the connection's key header is
missing or wrong; the user fixes it in the MCP client's configuration.
<!-- end -->

Never work around a failure by writing the report somewhere else; tell the
user the report was not filed and why.
