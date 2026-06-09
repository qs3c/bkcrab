# Tools Reference

## Built-in Tools

### exec
Execute shell commands with timeout and safety checks.
- Dangerous commands (rm -rf /, mkfs, etc.) are blocked
- Default timeout: 30 seconds

### read_file
Read file contents. Supports absolute paths or paths relative to the workspace.

### write_file
Write content to a file. Creates parent directories as needed.

### list_dir
List files and directories with size information.

### message
Send a message to a specific channel and chat ID.
