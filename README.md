# Slack Dumper (Fork)

> **This is a fork of [rusq/slackdump](https://github.com/rusq/slackdump)** with
> enhancements for generating Slack workspace context for AI agents. Changes in
> this fork focus on providing better visibility into which channels contain
> useful data and which can be safely excluded, so you can tune your exports to
> be faster and more relevant.
>
> **Fork enhancements:**
> - **Empty channel reporting**: After export, channels that returned zero
>   messages are listed with their IDs and names in exclusion-list format
>   (`^CHANNEL_ID`), ready to copy into an exclusion file.
> - **Richer progress output**: The progress bar now shows per-channel message
>   and thread counts alongside running totals, so you can see at a glance
>   which channels are contributing data.
> - **Result metadata**: The stream `Result` struct now carries `ChannelName`
>   and `MessageCount`, making it easier for downstream tooling and callbacks
>   to act on per-channel statistics without additional API calls.
> - **Automated export script** (`run_slack_export.sh`): A checkpoint-based
>   wrapper that exports only new messages since the last run and outputs
>   timestamped markdown files for AI agent consumption.

Purpose: archive your private and public Slack messages, users, channels,
files and emojis. Generate Slack Export without admin privileges.

**Links**:

- [Upstream project (rusq/slackdump)](https://github.com/rusq/slackdump)
- [Upstream releases](https://github.com/rusq/slackdump/releases/)
- [Upstream documentation](https://github.com/rusq/slackdump/blob/master/doc/README.md)

> [!WARNING]
> # Enterprise Workspaces Security Alerts
>
> Depending on your Slack plan and security settings, using Slackdump may
> trigger Slack security alerts and/or notify workspace administrators of
> unusual or automated access/scraping attempts.
>
> You are responsible for ensuring your use complies with your organisation's
> policies and Slack's terms of service.
>
> **See [Enterprise Workspace Tips](doc/enterprise.md).**

## Description

Typical use scenarios:

* archive your private conversations from Slack when the administrator
  does not allow you to install applications OR you don't want to use
  potentially privacy-violating third-party tools;
* archive channels from Slack when you're on a free "no archive" subscription;
  so you don't lose valuable knowledge in those channels;
* create a Slack Export archive without admin access;
* create incremental Slack archives, which is particularly useful for free
  workspaces with 90-day limits;
* save your favourite emojis; AND
* analyse your Slack data with AI Agents using [Slackdump MCP Server](#slackdump-mcp-server).

There are several modes of operation

1. List users/channels
1. Dumping messages and threads
1. Creating a Slack Export in Mattermost or Standard modes.
1. Creating an Archive (Sqlite database or stored as json+gz)
1. Converting an archive to other formats (Export, Dump).
1. Emoji download mode.
1. Viewing Slack export, dump or archive files or directories (displays images).
1. Search mode (messages and files).
1. Resuming previous archive (adding new messages to an existing archive).
1. Local MCP Server to use with Opencode, Claude, or any other AI tool
   supporting mcp over STDIO or HTTP.

Run `slackdump help` to see all available options.

## Installation

This fork does not publish pre-built releases. Build from source:

```shell
git clone https://github.com/Saboosh/slackdump.git
cd slackdump
go build -o slackdump ./cmd/slackdump
./slackdump wiz
```

Or run directly without building:

```shell
go run ./cmd/slackdump wiz
```

Note: You need Go installed on your system (see `go.mod` for the version).

For pre-built binaries of the upstream project, see [upstream releases](https://github.com/rusq/slackdump/releases/).

### Getting Help

- Quickstart guide: `slackdump help quickstart`
- [Ez-Login 3000](https://github.com/rusq/slackdump/wiki/EZ-Login-3000) Guide.
- What's new in V4: `slackdump help whatsnew`

## Automated Export Script (Fork Addition)

`run_slack_export.sh` is a wrapper that automates incremental Slack exports. It
tracks state via a checkpoint file so each run only fetches new messages since
the last successful export.

### Prerequisites

- `slackdump` built and available on `PATH` (or in the same directory)
- `secrets.txt` containing your `SLACK_TOKEN` (loaded via `-load-env`)
- `channels.txt` with channel exclusions (one `^CHANNEL_ID` per line)
- `process_export.py` to convert the export archive into markdown

### Usage

```shell
# First run — exports the last 24 hours (default)
./run_slack_export.sh

# First run — exports the last 72 hours
./run_slack_export.sh 72

# Subsequent runs — automatically resumes from the last checkpoint
./run_slack_export.sh
```

### How it works

1. Reads `.last_export_time` checkpoint (or falls back to `hours_back`).
2. Runs `slackdump export` for the window between the checkpoint and now.
3. Extracts the zip and runs `process_export.py` to produce a timestamped
   markdown file (`slack_content_YYYYMMDD_HHMM.md`).
4. Updates the checkpoint only after the full pipeline succeeds.

To force a re-export of a specific window, delete `.last_export_time` and run
with the desired `hours_back` argument.

# Previewing Results

Once the workspace data is dumped, you can run built-in viewer:

```shell
slackdump view <zip or directory>
```

The built-in viewer supports all types of dumps:

1. Slackdump Archive format;
1. Standard and Mattermost Slack Export;
1. Dump mode files

Alternatively, you can use one of the following tools to preview the
export results:

- [SlackLogViewer] - a fast and powerful Slack Export viewer written in C++, works on Export files (images won't be displayed, unless you used an export token flag).
- [Slackdump2Html] - a great Python application that converts Slack Dump to a
  static browsable HTML. It works on Dump mode files.
- [slack export viewer][slack-export-viewer] - Slack Export Viewer is a well known viewer for
  slack export files. Supports displaying files if saved in the "Standard" file mode.

[SlackLogViewer]: https://github.com/thayakawa-gh/SlackLogViewer/releases
[Slackdump2Html]: https://github.com/kununu/slackdump2html
[slack-export-viewer]: https://github.com/hfaran/slack-export-viewer

# Slackdump MCP server

Slackdump offers a read-only MCP server with the following features:
- analyse the data in the archive (any type)
- provide help with command line flags

Available MCP tools:

| Tool | Description |
|------|-------------|
| `load_source` | Open (or switch to) a Slackdump archive at runtime |
| `list_channels` | List all channels in the archive |
| `get_channel` | Get detailed info for a channel by ID |
| `list_users` | List all users/members |
| `get_messages` | Read messages from a channel (paginated) |
| `get_thread` | Read all replies in a thread |
| `get_workspace_info` | Workspace/team metadata |
| `command_help` | Get CLI flag help for any slackdump subcommand |

The server supports both **stdio** (agent-managed) and **HTTP** transports.

### Quick project setup

Scaffold a ready-to-use project directory pre-configured for your AI tool:

```shell
slackdump mcp -new opencode   ~/my-slack-project   # OpenCode
slackdump mcp -new claude-code ~/my-slack-project  # Claude Code
slackdump mcp -new copilot    ~/my-slack-project   # VS Code / GitHub Copilot
```

Each command creates the MCP config file and installs bundled Slackdump skill /
instruction files so the agent knows how to work with your archive out of the box.

To learn how to set it up with Claude Desktop, VS Code/GitHub Copilot, or
OpenCode, see:
```
slackdump help mcp
```

## FAQ

#### Do I need to create a Slack application?

No, you don't. Just run the application and EZ-Login 3000 will take
care of the authentication or, alternatively, grab that token and
cookie from the browser Slack session.

#### I'm getting "invalid_auth" error

Run `slackdump workspace new <name or url>` to reauthenticate.

#### How to read the export file?

```shell
slackdump view <ZIP-archive or directory>
```

#### My Slack Workspace is on the Free plan. Can I get data older than 90-days?

No, unfortunately you can't. Slack doesn't allow to export data older than 90
days for free workspaces, the API does not return any data before 90 days for
workspaces on the Free plan.

#### What's the difference between "archive", "export" and "dump"?

"Archive" is the new format introduced in v3, it minimises the memory use
while scraping the data and also has a universal structure that can be
converted into export and dump formats at will by using the "convert" command.

"Export" format aims to replicate the files generated when exporting a Slack
workspace for compatibility.

"Dump" format has one channel per file, there's no workspace information nor
any users stored. Should it be required, one must get users and channels by
running `slackdump list` command.

Behind the scenes slackdump always uses the "archive" file format for all
operations except "emoji" and "list", and converts to other formats on the
fly, removing the temporary archive files afterwards.

## License

Slackdump is licensed under the [GNU Affero General Public License v3.0 (AGPLv3)](LICENSE).

## Acknowledgments

This fork is based on [rusq/slackdump](https://github.com/rusq/slackdump) by
Rustam Gilyazov and contributors. See the upstream project for the full list of
contributors and sponsors.
