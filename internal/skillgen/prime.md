# AgentFeedback

AgentFeedback is set up on this machine. File a friction report when
something slowed you down:

- a document was missing, wrong or out of date;
- a tool, flag or command behaved unlike its name or its documentation;
- configuration pointed at something stale or absent;
- you needed several attempts at something that should have been written down.

File at the end of the task, once you know the fix; only real friction,
once per problem:

```bash
agentfeedback submit friction --category <word> --summary "<one line>" \
  --suggested-fix "<the fix>" --model "<your model id>"
```

The last line is a JSON outcome: `submitted`, `duplicate`, `spooled` and
`disabled` are all fine; do not file again. Never include credentials or
personal data. For what to include, the outcomes and what to do when it
fails, load the `agentfeedback` skill (`agentfeedback skill render
skill-md` prints it).
