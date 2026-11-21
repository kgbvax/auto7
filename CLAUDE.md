# CLAUDE.md for auto7

auto7 automates chasing in the digital modes FT8 and FT4. It reads the decodes of WSJT-X and MSHV, finds the stations that the operator needs, and tells the program which station to call. "Needed" means what chase-queue decides from its goals and the log database. RTTY can follow in a later version, then mostly for contests.

auto7 is a copy of auto8 (GitHub `opsotter/auto8`), made on 2026-11-21. auto8 builds its code with an automatic GitHub workflow. auto7 has no such workflow. The operator builds auto7 interactively with Claude Code and the compound-engineering plugin. auto7 belongs to the operating domain of opsotter (`~/dev/opsotter`, private, GitHub `kgbvax/opsotter`).

## How work happens here

Work happens in an interactive session with the operator. There are no issues, labels, build agents or merge gates. Use the skills of the compound-engineering plugin:

1. `ce-brainstorm` or `ce-ideate` to shape an idea. Ideas go to `docs/ideation/`.
2. `ce-plan` to write or deepen a plan in `docs/plans/`.
3. `ce-work` to build one unit or stage of a plan.
4. `ce-code-review` and `ce-simplify-code` to review the change.
5. `ce-commit` to commit. If the operator asks for a commit, use it.
6. `ce-compound` to record a lesson that the code does not show.

The current plan is `docs/plans/2026-11-20-2358-feat-auto7-automatic-hunter-plan.md`. It governs where it conflicts with the draft plan `docs/plans/2026-11-20-1003-feat-auto7-v1-plan.md`. `docs/ARCHITECTURE.md` holds the architecture and the tech stack.

These rules apply to every session:

- The operator owns the requirements. If the operator asks for it in the session, change the text of a requirement in a plan. Do not change it otherwise.
- Ask the operator before you change TX control. TX control is any code that can make WSJT-X or MSHV transmit. It is also the code that decides the time at which auto7 can ask for a transmission, such as the arm and presence logic. Keep TX control code inside `internal/tx`. Show the operator the diff of each TX control change before the commit.
- Never write code in another project. Another project is every repository other than `kgbvax/auto7`, for example auto8, opsotter (with chase-queue), horstreporter and StationA. A change to a vendored copy of their code, or to the MQTT topics and payloads that they read, also counts. Describe the change that auto7 needs and give the description to the operator.

## Writing rule

Write all text in this repository with the simple-english skill. This covers requirements, plans, documents, commit messages, code comments, and every user interface text. Reply in the output style `simple-english`.

The repository carries the output style in `.claude/output-styles/simple-english.md`, the skill in `.claude/skills/simple-english/`, and the linter in `tools/simple-english/`. `.claude/settings.json` selects the style and runs the linter as a hook. So every contributor who opens the repository in Claude Code gets all three without an install. The hook needs `python3`. It lints each Markdown file that you write. Fix every hit before you continue. The same configuration turns off the simple-english plugin in this repository, so that its hook does not run twice, and turns on the compound-engineering plugin.

## Rules for the code

- The opsotter rules apply (`~/dev/opsotter/CLAUDE.md`, StationA `docs/domains.md`). The station must operate with auto7 stopped. WSJT-X and MSHV must work as normal without auto7.
- auto7 never transmits by itself. It asks WSJT-X or MSHV to call a station, and only while the operator is present and auto7 is armed. Armed means that the operator turned on automatic calls in auto7. The presence heartbeat is a regular message from logotter. It shows that the operator is at the station. The operator can stop auto7 at once. If the presence heartbeat stops, auto7 stops by itself.
- auto7 does not evaluate awards. chase-queue (opsotter `operating/chase-queue`) owns the award and goal logic. auto7 sends its decoded stations to chase-queue and reads the answer from the `/state` of chase-queue.
- auto7 talks to the station only over the MQTT bus contract (StationA `docs/station-integration-model.md`). It talks to WSJT-X and MSHV over their UDP protocol on the PC where they run. It does not change the radio, the PA or the antennas.
- Never commit callsign logs, QSO data, host addresses, credentials, or configuration files with secrets. Secrets come only from the environment (for example `AUTO7_MQTT_PASSWORD`). They never go into the TOML file, the logs or the MQTT payloads.
- Every user interface follows opsotter `docs/DESIGN.md`.
- If auto7 needs StationA `shared`, import `codeberg.org/kgbvax/stationa/shared` or vendor a copy. Never import opsotter code.
- Before you report a change as complete, run `go build ./...`, `go vet ./...` and `go test -race ./...`.
- Deploy only on a request of the operator, every time.

## Layout

| Path | Role |
|---|---|
| `cmd/auto7/` | The command (a stub today) |
| `internal/tx/` | TX control code (empty today) |
| `docs/ARCHITECTURE.md` | The architecture and the tech stack |
| `docs/plans/` | Requirements and plans (unified plans of the compound-engineering plugin) |
| `docs/ideation/` | Ideas from `ce-ideate` |
| `tools/simple-english/` | The writing linter |

## Commands

```bash
go build ./...
go vet ./...
go test -race ./...
python3 tools/simple-english/ste_lint.py --type descriptive CLAUDE.md
```
