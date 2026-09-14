# Windows Notes

- Spawning many processes in a burst can attract Windows Defender's
  attention; consider an exclusion for your `forebay.exe` and
  `%USERPROFILE%\.forebay` if you see slow starts.
- Globs are matched case-insensitively deduped, since the filesystem is
  case-insensitive.
