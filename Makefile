.PHONY: build build-web bundle-skills clean release-local install test dev

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
# 将构建身份标记到主包（旧的 `bkcrab version` 调用者）
# 和 internal/buildinfo（agent 运行时 + 系统提示读取器）中。
# 从一个 VERSION 变量保持两者同步意味着发布构建向模型提供
# 与 CLI 报告相同的字符串。
BUILDINFO = github.com/qs3c/bkcrab/internal/buildinfo
LDFLAGS  = -s -w \
	-X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE) \
	-X $(BUILDINFO).Version=$(VERSION) -X $(BUILDINFO).Commit=$(COMMIT) -X $(BUILDINFO).Date=$(DATE)

# 安装目标。默认是用户级别的 XDG 风格 bin 目录，因此简单的
# `make install` 不需要 sudo。可通过以下方式覆盖：
#   make install PREFIX=/usr/local        （系统范围；需要 sudo）
#   make install PREFIX=/opt/homebrew     （Apple Silicon brew 布局）
PREFIX ?= $(HOME)/.local

build-web:
	cd web && pnpm install --frozen-lockfile && pnpm build
	rm -rf internal/setup/web
	cp -r web/out internal/setup/web

# bundle-skills 将二进制文件应随附的技能同步到
# internal/agent/bundled_skills/ 下的嵌入树中。真实源位于
# 仓库根目录 skills/<name>/，以便在一个地方编辑；此目标
# 在每次构建时覆盖嵌入副本，从而避免累积偏差。
# `go:embed` 无法跟随符号链接或转义包目录，因此
# 真实副本是唯一可行的方式。
bundle-skills:
	@rm -rf internal/agent/bundled_skills/skill-creator
	@cp -R skills/skill-creator internal/agent/bundled_skills/skill-creator
	@rm -rf internal/agent/bundled_skills/find-skills
	@cp -R skills/find-skills internal/agent/bundled_skills/find-skills
	@echo "==> bundled skills synced"

build: build-web bundle-skills
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/bkcrab ./cmd/bkcrab

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/bkcrab $(PREFIX)/bin/bkcrab
	@echo
	@echo "==> installed: $(PREFIX)/bin/bkcrab"
	@case ":$$PATH:" in *":$(PREFIX)/bin:"*) ;; *) \
	  echo "    NOTE: $(PREFIX)/bin is not on your PATH."; \
	  echo "    Add to ~/.zshrc:  export PATH=\"$(PREFIX)/bin:\$$PATH\"" ;; \
	esac

test:
	go test ./...

dev: build-web
	air

clean:
	rm -rf bin/ dist/ tmp/

# 构建所有平台
release-local: build-web bundle-skills
	@mkdir -p dist
	@# macOS 系统
	GOOS=darwin  GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/bkcrab_darwin_arm64/bkcrab  ./cmd/bkcrab
	GOOS=darwin  GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/bkcrab_darwin_amd64/bkcrab  ./cmd/bkcrab
	@# Linux 系统
	GOOS=linux   GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/bkcrab_linux_arm64/bkcrab   ./cmd/bkcrab
	GOOS=linux   GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/bkcrab_linux_amd64/bkcrab   ./cmd/bkcrab
	@# Windows 系统
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/bkcrab_windows_amd64/bkcrab.exe ./cmd/bkcrab
	GOOS=windows GOARCH=arm64 CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o dist/bkcrab_windows_arm64/bkcrab.exe ./cmd/bkcrab
	@# 打包：unix 用 tar.gz，windows 用 zip
	@cd dist && for d in bkcrab_darwin_* bkcrab_linux_*; do tar -czf "$${d}.tar.gz" -C "$$d" bkcrab; done
	@cd dist && for d in bkcrab_windows_*; do (cd "$$d" && zip -q "../$${d}.zip" bkcrab.exe); done
	@echo "Release artifacts:"
	@ls -lh dist/*.tar.gz dist/*.zip 2>/dev/null
