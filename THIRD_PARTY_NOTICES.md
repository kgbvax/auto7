# Third-party notices

## Simple English output style and skill

These files are copies from the Simple English plugin, version 2.1.1 (https://github.com/AminBlg/SimpleEnglish):

- `.claude/output-styles/simple-english.md`, a copy of `output-styles/simple-english.md`.
- `.claude/skills/simple-english/`, a copy of `skills/simple-english/` (`SKILL.md` and its `references`).
- `tools/simple-english/ste_lint.py` and `tools/simple-english/slop.tsv`, copies of the same files in `evals/`.
- `tools/simple-english/lint_hook.py`, a copy of `src/hooks/lint_hook.py`. auto7 changed one line: the hook loads `ste_lint.py` from its own directory, not from `evals/`.

They are under the MIT License:

```text
MIT License

Copyright (c) 2026 AminBlg

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## StationA shared packages

`third_party/stationa-shared/` is a copy of the `shared` module of StationA (Codeberg `kgbvax/stationa`). auto7 uses it as the module `codeberg.org/kgbvax/stationa/shared`, with a `replace` line in `go.mod`, because Codeberg is not reachable from the build.

- Source: `shared/` of StationA, git commit `78bff02` of 2026-11-14.
- Copied: `LICENSE`, `go.mod`, `go.sum`, and the packages `mqtt`, `schema` and `logging`, with their tests.
- Not copied: the packages `antmode` and `pstrotator`, because auto7 does not use them.
- auto7 did not change the copied files. A change to them is a change in StationA.

The packages are under the GNU Affero General Public License, version 3 or later (`AGPL-3.0-or-later`). The full text is in `third_party/stationa-shared/LICENSE`.
