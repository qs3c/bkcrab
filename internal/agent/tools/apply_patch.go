package tools

// apply_patch — 与 OpenAI Codex 的 DSL 一致的多文件补丁工具。
//
// 一次工具调用即可添加、更新、删除或重命名任意数量的文件。
// 两阶段执行：解析信封并计算每个文件的新值
// 首先内存中的内容；只有当每个帅哥主播都成功的时候
// 我们刷新写入/删除。如果任何块失败，磁盘上的文件不会发生变化 -
// 代理收到明显错误并可以重新发出补丁。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/qs3c/bkclaw/internal/sandbox"
)

// -----------------------------------------------------------------------------
// 抽象语法树（AST）
// -----------------------------------------------------------------------------

type patchOpType int

const (
	opAdd patchOpType = iota
	opUpdate
	opDelete
)

type hunkLineKind int

const (
	lineContext hunkLineKind = iota
	lineAdd
	lineRemove
)

type hunkLine struct {
	Kind hunkLineKind
	Text string // without the leading +/-/space marker
}

type hunk struct {
	Lines []hunkLine
	IsEOF bool // hunk anchors to end of file
}

type patchOp struct {
	Type    patchOpType
	Path    string
	MoveTo  string // Update only — empty when no rename
	AddBody string // Add only — literal new file contents
	Hunks   []hunk // Update only
}

type patch struct {
	Ops []patchOp
}

const (
	beginPatch   = "*** Begin Patch"
	endPatch     = "*** End Patch"
	addPrefix    = "*** Add File: "
	updatePrefix = "*** Update File: "
	deletePrefix = "*** Delete File: "
	moveToPrefix = "*** Move to: "
	endOfFile    = "*** End of File"
	hunkSep      = "@@"
)

// -----------------------------------------------------------------------------
// 解析器
// -----------------------------------------------------------------------------

// parsePatch 将补丁包络转换为结构化 AST。错误包括
// 有问题的线，以便模型可以自我纠正。
func parsePatch(input string) (*patch, error) {
	trimmed := strings.TrimSpace(input)
	if !strings.HasPrefix(trimmed, beginPatch) {
		return nil, fmt.Errorf("apply_patch: input must start with %q", beginPatch)
	}

	lines := strings.Split(trimmed, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}

	p := &patch{}
	var (
		currentOp   *patchOp
		currentHunk *hunk
		seenEnd     bool
	)

	flushOp := func() {
		if currentOp == nil {
			return
		}
		if currentHunk != nil {
			currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
			currentHunk = nil
		}
		p.Ops = append(p.Ops, *currentOp)
		currentOp = nil
	}

loop:
	for i := 1; i < len(lines); i++ {
		line := lines[i]

		switch {
		case strings.TrimSpace(line) == endPatch:
			flushOp()
			seenEnd = true
			break loop

		case strings.HasPrefix(line, addPrefix):
			flushOp()
			currentOp = &patchOp{Type: opAdd, Path: strings.TrimSpace(line[len(addPrefix):])}

		case strings.HasPrefix(line, updatePrefix):
			flushOp()
			currentOp = &patchOp{Type: opUpdate, Path: strings.TrimSpace(line[len(updatePrefix):])}

		case strings.HasPrefix(line, deletePrefix):
			flushOp()
			currentOp = &patchOp{Type: opDelete, Path: strings.TrimSpace(line[len(deletePrefix):])}

		case strings.HasPrefix(line, moveToPrefix):
			if currentOp == nil || currentOp.Type != opUpdate {
				return nil, fmt.Errorf("apply_patch: %q outside an Update File block", strings.TrimSpace(line))
			}
			if currentOp.MoveTo != "" {
				return nil, fmt.Errorf("apply_patch: duplicate Move to: for %q", currentOp.Path)
			}
			if len(currentOp.Hunks) > 0 || currentHunk != nil {
				return nil, fmt.Errorf("apply_patch: %q must come before any hunk in Update File: %q",
					strings.TrimSpace(line), currentOp.Path)
			}
			currentOp.MoveTo = strings.TrimSpace(line[len(moveToPrefix):])

		case strings.TrimSpace(line) == endOfFile:
			if currentOp == nil || currentOp.Type != opUpdate || currentHunk == nil {
				return nil, fmt.Errorf("apply_patch: %q not inside an Update hunk", endOfFile)
			}
			currentHunk.IsEOF = true
			currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
			currentHunk = nil

		case strings.HasPrefix(line, hunkSep):
			if currentOp == nil || currentOp.Type != opUpdate {
				return nil, fmt.Errorf("apply_patch: %q outside an Update File block", hunkSep)
			}
			if currentHunk != nil {
				currentOp.Hunks = append(currentOp.Hunks, *currentHunk)
			}
			currentHunk = &hunk{}

		default:
			if currentOp == nil {
				if strings.TrimSpace(line) == "" {
					continue
				}
				return nil, fmt.Errorf("apply_patch: unexpected line outside any file block: %q", line)
			}
			switch currentOp.Type {
			case opAdd:
				if !strings.HasPrefix(line, "+") {
					return nil, fmt.Errorf("apply_patch: Add File body line must start with %q (got %q)", "+", line)
				}
				// 每 + 行贡献一行文件内容。附加一个
				// 尾随换行符，因此文件以 \n 结尾（POSIX 约定，
				// 与大多数工具发出的内容相匹配）。
				currentOp.AddBody += line[1:] + "\n"
			case opDelete:
				if strings.TrimSpace(line) != "" {
					return nil, fmt.Errorf("apply_patch: Delete File expects no body (got %q)", line)
				}
			case opUpdate:
				if currentHunk == nil {
					currentHunk = &hunk{}
				}
				if line == "" {
					// 块内的空白行被视为空白
					// 上下文线。严格的法典需要“”（空格），但是
					// LLM 经常会删除前缀；容忍是无害的。
					currentHunk.Lines = append(currentHunk.Lines, hunkLine{Kind: lineContext, Text: ""})
					continue
				}
				switch line[0] {
				case ' ':
					currentHunk.Lines = append(currentHunk.Lines, hunkLine{Kind: lineContext, Text: line[1:]})
				case '+':
					currentHunk.Lines = append(currentHunk.Lines, hunkLine{Kind: lineAdd, Text: line[1:]})
				case '-':
					currentHunk.Lines = append(currentHunk.Lines, hunkLine{Kind: lineRemove, Text: line[1:]})
				default:
					return nil, fmt.Errorf("apply_patch: hunk line must start with ' ', '+', or '-' (got %q)", line)
				}
			}
		}
	}

	if !seenEnd {
		return nil, fmt.Errorf("apply_patch: missing %q sentinel", endPatch)
	}
	if len(p.Ops) == 0 {
		return nil, errors.New("apply_patch: empty patch (no file operations)")
	}
	return p, nil
}

// -----------------------------------------------------------------------------
// 应用程序（纯函数）
// -----------------------------------------------------------------------------

// applyHunks 将 Update 的所有块应用到 oldContent 并返回
// 新内容。保留尾随换行状态。第一个大块头那个
// 无法锚定会产生提及其预期行的错误。
func applyHunks(path, oldContent string, hunks []hunk) (string, error) {
	hadTrailingNL := strings.HasSuffix(oldContent, "\n")
	var lines []string
	if oldContent != "" {
		lines = strings.Split(strings.TrimSuffix(oldContent, "\n"), "\n")
	}

	// searchFrom 前进超过每个应用的大块，因此连续的大块不能
	// 重新匹配到已经重写的区域。
	searchFrom := 0
	for hi, h := range hunks {
		pattern := patternLines(h)
		// 纯添加块（只有“+”行，没有上下文，没有删除）有一个
		// 空图案。根据 Codex 规范，这些都固定在末尾
		// 当前文件的 - 最重要的是，它们不会推进
		// 搜索光标，因此稍后锚定的块仍然可以匹配较早的
		// 在文件中。
		anchorEOF := h.IsEOF || len(pattern) == 0

		idx := findHunkAnchor(lines, pattern, anchorEOF, searchFrom)
		if idx < 0 {
			return "", fmt.Errorf(
				"apply_patch: hunk #%d in %s did not match — re-read the file and emit a fresh patch.\nExpected lines:\n%s",
				hi+1, path, strings.Join(pattern, "\n"))
		}

		// 使用文件的*实际*文本作为上下文构建替换
		// 行（因此成功的模糊/Unicode 规范化匹配
		// 保留文件的空白和原始字形而不是
		// 用补丁的 ASCII 版本覆盖）。相当于
		// 当匹配完全时，replacementLines(h)。
		replacement := buildReplacement(h, lines, idx)

		next := make([]string, 0, len(lines)-len(pattern)+len(replacement))
		next = append(next, lines[:idx]...)
		next = append(next, replacement...)
		next = append(next, lines[idx+len(pattern):]...)
		lines = next
		// 仅将光标移动到锚定的帅哥。 EOF 锚定和
		// 纯添加块附加到文件末尾，并且不能
		// 限制后续锚定帅哥的搜索位置。
		if !anchorEOF {
			searchFrom = idx + len(replacement)
		}
	}

	out := strings.Join(lines, "\n")
	if hadTrailingNL && out != "" {
		out += "\n"
	}
	return out, nil
}

// patternLines 提取文件中必须匹配的模式：
// 上下文+按顺序删除行。
func patternLines(h hunk) []string {
	out := make([]string, 0, len(h.Lines))
	for _, l := range h.Lines {
		if l.Kind == lineContext || l.Kind == lineRemove {
			out = append(out, l.Text)
		}
	}
	return out
}

// buildReplacement 组装应替换匹配的行
// 地区。上下文行源自文件（保留文件的
// 当模糊匹配压缩差异时有自己的空白）；添加行来
// 来自猛男；删除线被丢弃。 fileLines[startIdx:] 必须
// 已经符合帅哥的模式了。
func buildReplacement(h hunk, fileLines []string, startIdx int) []string {
	out := make([]string, 0, len(h.Lines))
	off := 0 // cursor into the matched region in fileLines
	for _, l := range h.Lines {
		switch l.Kind {
		case lineContext:
			out = append(out, fileLines[startIdx+off])
			off++
		case lineRemove:
			off++
		case lineAdd:
			out = append(out, l.Text)
		}
	}
	return out
}

// searchSequence 返回模式对齐处的最小索引 ≥ start
// 与干草堆，或-1。允许使用空模式并在开始处锚定
// （由文件顶部的纯添加块使用或与 EOF 锚一起使用）。
func seekSequence(haystack, pattern []string, start int) int {
	if len(pattern) == 0 {
		if start <= len(haystack) {
			return start
		}
		return -1
	}
	for i := start; i+len(pattern) <= len(haystack); i++ {
		if linesEqual(haystack, i, pattern) {
			return i
		}
	}
	return -1
}

func linesEqual(haystack []string, start int, pattern []string) bool {
	if start < 0 || start+len(pattern) > len(haystack) {
		return false
	}
	for j, p := range pattern {
		if haystack[start+j] != p {
			return false
		}
	}
	return true
}

// findHunkAnchor 在“lines”中定位一个帅哥的模式，逐步尝试
// 更宽松的转换：identity→rstrip→fulltrim→Unicode
// 正常化。 EOF 锚定的帅哥更喜欢文件尾对齐，但会失败
// 返回到从 searchFrom 开始的正向扫描（与 Codex 的eek_sequence 匹配）。
// 当没有转换找到匹配项时返回 -1。
//
// 转换是逐行进行的，不会改变行数，因此
// 返回的索引对于原始“lines”数组有效。来电者
// 将该索引传递给 buildReplacement，以便上下文行源自
// 文件的实际（非标准化）文本 - 允许模糊匹配
// 字形差异而无需重写它们。
func findHunkAnchor(lines, pattern []string, anchorEOF bool, searchFrom int) int {
	transforms := []func(string) string{
		nil,               // identity
		rstripWS,          // trailing whitespace tolerance
		strings.TrimSpace, // full whitespace tolerance
		normalizeForFuzzy, // Unicode dashes/quotes/spaces → ASCII
	}
	for _, t := range transforms {
		var tl, tp []string
		if t == nil {
			tl, tp = lines, pattern
		} else {
			tl = mapLines(lines, t)
			tp = mapLines(pattern, t)
		}
		if anchorEOF {
			start := len(tl) - len(tp)
			if start >= 0 && linesEqual(tl, start, tp) {
				return start
			}
			// EOF 位置缺失→向前扫描，匹配 Codex 行为。
		}
		if idx := seekSequence(tl, tp, searchFrom); idx >= 0 {
			return idx
		}
	}
	return -1
}

func mapLines(in []string, f func(string) string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = f(s)
	}
	return out
}

func rstripWS(s string) string {
	return strings.TrimRightFunc(s, unicode.IsSpace)
}

// NormalizeForFuzzy 映射 LLM（或自动更正）的印刷字形
// 编辑器）经常替换其 ASCII 对应项。后
// 映射时，该线也被完全修剪，因此该级别包含
// 当差异恰好是空白和
// 字形。镜像 Codex 的eek_sequence 中的 Unicode 映射。
func normalizeForFuzzy(s string) string {
	if isASCII(s) {
		return strings.TrimSpace(s)
	}
	var sb strings.Builder
	sb.Grow(len(s))
	for _, r := range s {
		switch r {
		// 破折号和连字符（en/em/figure/non-break/minus/small/fullwidth）。
		case '‐', '‑', '‒', '–', '—', '―',
			'−', '﹘', '﹣', '－':
			sb.WriteByte('-')
		// 单引号/撇号（左、右、低位、高位反转）。
		case '‘', '’', '‚', '‛':
			sb.WriteByte('\'')
		// 双引号（左、右、低位、高位反转）。
		case '“', '”', '„', '‟':
			sb.WriteByte('"')
		// 各种空格（NBSP、图形、标点符号、细、头发、窄
		// 不间断、中等数学、表意文字）。
		case ' ', ' ', ' ', ' ', ' ',
			' ', ' ', '　':
			sb.WriteByte(' ')
		default:
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------------------------
// 工具描述/架构
// -----------------------------------------------------------------------------

const applyPatchDescription = `Apply a multi-file patch in OpenAI Codex DSL format. Use this instead of chained edit_file/write_file calls when a change touches ≥2 files or ≥2 hunks — one tool call performs every edit atomically (parse + hunk matching happens for every file before any write; if any hunk fails to anchor, NO file is modified).

Format:

  *** Begin Patch
  *** Add File: path/new.go
  +line one
  +line two
  *** Update File: path/old.go
  *** Move to: path/renamed.go    (optional rename, before any hunk)
  @@
   keep_this_line
  -drop_this
  +add_this
   keep_this_too
  @@
   second_anchor
  -bye
  +hi
  *** End of File                  (optional; pin the previous hunk to file end)
  *** Delete File: path/legacy.go
  *** End Patch

Rules:
- Hunks anchor on context lines (' ' prefix) plus '-' lines that must literally match the file. Provide enough context to make the location unambiguous; matching is in-order, first match wins.
- Pure-add hunks (only '+' lines) only work with *** End of File or at the very top of a file.
- Identity files (SOUL.md, IDENTITY.md, MEMORY.md, AGENTS.md, BOOTSTRAP.md, TOOLS.md, HEARTBEAT.md, USER.md, agent.json) accept Add and Update but NOT Delete or Move.
- Path resolution matches read_file/write_file: workspace-relative paths go to the workspace store, identity-file basenames go to the system store, absolute paths go to disk.`

var applyPatchSchema = map[string]interface{}{
	"type": "object",
	"properties": map[string]interface{}{
		"input": map[string]interface{}{
			"type":        "string",
			"description": "The complete patch envelope from `*** Begin Patch` to `*** End Patch`.",
		},
	},
	"required": []string{"input"},
}

type applyPatchArgs struct {
	Input string `json:"input"`
}

// -----------------------------------------------------------------------------
// 后端助手 — 主机文件系统模式（镜像 registerFile 的路由）
// -----------------------------------------------------------------------------

func (r *Registry) readForPatch(ctx context.Context, path string) (string, error) {
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.sessionID, path)
		if err != nil {
			return "", fmt.Errorf("workspace get: %w", err)
		}
		defer rc.Close()
		data, err := io.ReadAll(rc)
		if err != nil {
			return "", fmt.Errorf("workspace read: %w", err)
		}
		return string(data), nil
	}
	if r.systemFileStore != nil && r.agentID != "" && basenameIsSystemFile(path) {
		name := filepath.Base(filepath.Clean(path))
		if data, err := r.readSystemFileForUser(ctx, r.systemFileUserID(name), name); err == nil {
			return string(data), nil
		}
		if r.systemRoot != "" {
			if data, err := os.ReadFile(filepath.Join(r.systemRoot, name)); err == nil {
				return string(data), nil
			}
		}
		return "", nil
	}
	root := r.rootForPath(path)
	full, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) && isSingleSegmentSystemFile(path) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(data), nil
}

func (r *Registry) writeForPatch(ctx context.Context, path, content string) error {
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		return r.workspaceStore.Put(ctx, r.agentID, r.projectID, r.sessionID, path,
			strings.NewReader(content), int64(len(content)), "")
	}
	if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(path) {
		name := filepath.Clean(path)
		if err := r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.systemFileUserID(name), name, []byte(content)); err != nil {
			return err
		}
		// 镜像到磁盘，以便该 Pod 的进程内读取器（上下文生成器、
		// 技能加载器）立即看到新内容。与以下相同的不变量
		// makeWriteFile。
		if r.systemRoot != "" {
			disk := filepath.Join(r.systemRoot, name)
			_ = os.MkdirAll(filepath.Dir(disk), 0o755)
			_ = os.WriteFile(disk, []byte(content), 0o644)
		}
		return nil
	}
	root := r.rootForPath(path)
	full, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), path)
	if err != nil {
		return err
	}
	if isGlobalSkillsPath(full) {
		return errGlobalSkillsDirWrite
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s: %w", path, err)
	}
	return os.WriteFile(full, []byte(content), 0o644)
}

func (r *Registry) deleteForPatch(ctx context.Context, path string) error {
	// 身份文件拒绝删除：systemFileStore没有删除API并且
	// 这些文件有一个固定的槽 - 通过删除清除它们
	// 腐败代理人。使用目标内容为空的更新文件
	// 反而。
	if isSingleSegmentSystemFile(path) {
		return fmt.Errorf("apply_patch: refusing to delete identity file %q (use Update File with empty content instead)", path)
	}
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		return r.workspaceStore.Delete(ctx, r.agentID, r.projectID, r.sessionID, path)
	}
	root := r.rootForPath(path)
	full, err := resolvePathSandboxed(root, r.effectiveSandboxRoot(root), path)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		return fmt.Errorf("delete %s: %w", path, err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// 后端助手——沙箱模式（镜像registerSandboxedFile的路由）
// -----------------------------------------------------------------------------

func (r *Registry) readForPatchSandbox(ctx context.Context, ex sandbox.Executor, path string) (string, error) {
	if r.systemFileStore != nil && r.agentID != "" && basenameIsSystemFile(path) {
		name := filepath.Base(filepath.Clean(path))
		if data, err := r.readSystemFileForUser(ctx, r.systemFileUserID(name), name); err == nil {
			return string(data), nil
		}
		return "", nil
	}
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		rc, err := r.workspaceStore.Get(ctx, r.agentID, r.projectID, r.sessionID, path)
		if err == nil {
			defer rc.Close()
			data, readErr := io.ReadAll(rc)
			if readErr == nil {
				return string(data), nil
			}
		}
		// 发生存储丢失/读取错误时，会落入执行程序。
	}
	return ex.ReadFile(ctx, path)
}

func (r *Registry) writeForPatchSandbox(ctx context.Context, ex sandbox.Executor, path, content string) error {
	if r.systemFileStore != nil && r.agentID != "" && isSingleSegmentSystemFile(path) {
		name := filepath.Clean(path)
		return r.systemFileStore.SaveWorkspaceFile(ctx, r.agentID, r.systemFileUserID(name), name, []byte(content))
	}
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		return r.workspaceStore.Put(ctx, r.agentID, r.projectID, r.sessionID, path,
			strings.NewReader(content), int64(len(content)), "")
	}
	_, err := ex.WriteFile(ctx, path, content)
	return err
}

func (r *Registry) deleteForPatchSandbox(ctx context.Context, ex sandbox.Executor, path string) error {
	if isSingleSegmentSystemFile(path) {
		return fmt.Errorf("apply_patch: refusing to delete identity file %q (use Update File with empty content instead)", path)
	}
	if r.workspaceStore != nil && r.agentID != "" && r.isWorkspacePath(path) {
		return r.workspaceStore.Delete(ctx, r.agentID, r.projectID, r.sessionID, path)
	}
	// 沙盒执行器不公开删除 API；回到“rm”。单引号
	// 路径和转义嵌入单引号，因此是病态的文件名
	// 无法注入shell。
	q := "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	_, err := ex.Exec(ctx, "rm -f -- "+q, 0)
	return err
}

// -----------------------------------------------------------------------------
// 工具注册
// -----------------------------------------------------------------------------

func registerApplyPatch(r *Registry) {
	r.Register("apply_patch", applyPatchDescription, applyPatchSchema, func(ctx context.Context, raw json.RawMessage) (string, error) {
		var args applyPatchArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("apply_patch: parse args: %w", err)
		}
		return runApplyPatch(ctx, args.Input,
			func(ctx context.Context, p string) (string, error) { return r.readForPatch(ctx, p) },
			func(ctx context.Context, p, c string) error { return r.writeForPatch(ctx, p, c) },
			func(ctx context.Context, p string) error { return r.deleteForPatch(ctx, p) },
		)
	})
}

func registerSandboxedApplyPatch(r *Registry, ex sandbox.Executor) {
	r.Register("apply_patch", applyPatchDescription, applyPatchSchema, func(ctx context.Context, raw json.RawMessage) (string, error) {
		var args applyPatchArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("apply_patch: parse args: %w", err)
		}
		out, err := runApplyPatch(ctx, args.Input,
			func(ctx context.Context, p string) (string, error) { return r.readForPatchSandbox(ctx, ex, p) },
			func(ctx context.Context, p, c string) error { return r.writeForPatchSandbox(ctx, ex, p, c) },
			func(ctx context.Context, p string) error { return r.deleteForPatchSandbox(ctx, ex, p) },
		)
		if err != nil {
			return "", err
		}
		return MetaSandboxPrefix + out, nil
	})
}

// runApplyPatch 是与存储无关的引擎。第一阶段：解析和计算
// 每个文件的新内容/计划在内存中删除。第 2 阶段：仅在之后
// 所有帅哥锚定成功，刷新写入（然后删除 - 移动源
// 最后走，以便在释放旧插槽之前目的地已就位）。
//
// 第 2 阶段错误泄漏部分状态（一个文件写入，下一个文件失败）；
// 修复根本原因后，代理可以重新发出补丁
// （权限、磁盘已满，...）。跨后端事务回滚是
// 没有尝试过——这需要对每个受影响的商店进行快照。
func runApplyPatch(
	ctx context.Context,
	input string,
	read func(context.Context, string) (string, error),
	write func(context.Context, string, string) error,
	del func(context.Context, string) error,
) (string, error) {
	p, err := parsePatch(input)
	if err != nil {
		return "", err
	}

	type plannedWrite struct{ path, content string }
	var (
		writes  []plannedWrite
		deletes []string
	)

	for _, op := range p.Ops {
		switch op.Type {
		case opAdd:
			if op.Path == "" {
				return "", errors.New("apply_patch: Add File requires a non-empty path")
			}
			writes = append(writes, plannedWrite{op.Path, op.AddBody})

		case opDelete:
			if op.Path == "" {
				return "", errors.New("apply_patch: Delete File requires a non-empty path")
			}
			// 身份文件在 systemFileStore 中有一个固定的槽，并且
			// 无删除API；允许在此处删除会损坏代理。
			// 在引擎级别拒绝，因此后端 del() 不必
			// 重复规则（纵深防御仍然适用）
			// 删除ForPatch /删除ForPatchSandbox）。
			if isSingleSegmentSystemFile(op.Path) {
				return "", fmt.Errorf("apply_patch: refusing to delete identity file %q (use Update File with empty content instead)", op.Path)
			}
			deletes = append(deletes, op.Path)

		case opUpdate:
			if op.Path == "" {
				return "", errors.New("apply_patch: Update File requires a non-empty path")
			}
			if op.MoveTo != "" && (isSingleSegmentSystemFile(op.Path) || isSingleSegmentSystemFile(op.MoveTo)) {
				return "", fmt.Errorf("apply_patch: refusing to Move identity file %q → %q", op.Path, op.MoveTo)
			}
			old, err := read(ctx, op.Path)
			if err != nil {
				return "", fmt.Errorf("apply_patch: read %s: %w", op.Path, err)
			}
			updated, err := applyHunks(op.Path, old, op.Hunks)
			if err != nil {
				return "", err
			}
			target := op.Path
			if op.MoveTo != "" && op.MoveTo != op.Path {
				target = op.MoveTo
				deletes = append(deletes, op.Path)
			}
			writes = append(writes, plannedWrite{target, updated})
		}
	}

	for _, w := range writes {
		if err := write(ctx, w.path, w.content); err != nil {
			return "", fmt.Errorf("apply_patch: write %s: %w", w.path, err)
		}
	}
	for _, d := range deletes {
		if err := del(ctx, d); err != nil {
			return "", fmt.Errorf("apply_patch: delete %s: %w", d, err)
		}
	}

	var sb strings.Builder
	for _, op := range p.Ops {
		switch op.Type {
		case opAdd:
			fmt.Fprintf(&sb, "A %s\n", op.Path)
		case opDelete:
			fmt.Fprintf(&sb, "D %s\n", op.Path)
		case opUpdate:
			if op.MoveTo != "" && op.MoveTo != op.Path {
				fmt.Fprintf(&sb, "M %s -> %s (%d hunk(s))\n", op.Path, op.MoveTo, len(op.Hunks))
			} else {
				fmt.Fprintf(&sb, "U %s (%d hunk(s))\n", op.Path, len(op.Hunks))
			}
		}
	}
	return sb.String(), nil
}
