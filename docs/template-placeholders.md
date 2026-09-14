# Template Placeholders

In `forebay batch` templates, each matched file
substitutes:

| Placeholder | Example (root `C:\proj`) |
| --- | --- |
| `{path}` | `C:\proj\src\auth.js` (absolute, native separators) |
| `{slashpath}` | `C:/proj/src/auth.js` (absolute, forward slashes - safe inside prompts and JSON on Windows) |
| `{relpath}` | `src/auth.js` |
| `{name}` / `{base}` | `auth.js` / `auth` |
| `{dir}` | `C:\proj\src` |

`**/node_modules/**` and `**/.git/**` are excluded by default; pass your
own `--exclude` to override. Use `--dry-run` to preview the expansion
before queueing.
