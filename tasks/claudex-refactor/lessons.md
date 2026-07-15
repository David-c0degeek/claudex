- 2026-07-15 — A streaming subprocess can still deadlock if the coordinator
  writes a large stdin prompt synchronously before it starts draining stdout and
  stderr. Start both readers and a stdin writer before waiting; persist each raw
  line before decoding so malformed provider data cannot erase evidence.
