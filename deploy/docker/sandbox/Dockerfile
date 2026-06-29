# bkcrab-sandbox — agent 执行沙箱的运行时镜像。
#
# 为什么存在：默认的 `python:3.12-slim` 镜像只有 Python。
# 捆绑的技能（如 camoufox-cli）驱动真正的浏览器，因此我们需要
# 预装 Python + Node + 浏览器二进制文件，以便 agent 可以在
# 第一回合零往返地运行 `camoufox-cli open <url>`。
#
# 基础镜像：`node:22-bookworm-slim` — 已预装 Node 的小型 Debian 镜像。
# 我们在其之上添加 Python，让 camoufox-cli 通过
# `camoufox-cli install --with-deps` 拉取自己的反检测 Firefox（Camoufox）。

FROM node:22-bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# 系统依赖：Python 工具链 + 常规的获取/证书必需品 +
# CJK/emoji 字体，以便 Firefox 可以在中文/日文/韩文页面
# 和包含表情符号的网站上渲染真实字形而非豆腐块（.notdef）。
# 需要 `sudo`，因为 camoufox-cli 的 `install --with-deps` 即使在
# 调用者已经是 root 时也会调用 `sudo apt-get …` 来获取 Firefox 运行时库。
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        python3 \
        python3-pip \
        python3-venv \
        ca-certificates \
        curl \
        git \
        sudo \
        fonts-noto-cjk \
        fonts-noto-color-emoji \
 && ln -sf /usr/bin/python3 /usr/local/bin/python

# camoufox-cli + uv（提供 `uv` 和 `uvx`）+ 常用 Python 工具。
# Debian Bookworm 在系统 Python 上强制实施 PEP 668；
# `--break-system-packages` 是有文档记载的逃生口，在容器内是安全的。
RUN pip install --break-system-packages --no-cache-dir \
        camoufox-cli \
        uv \
        requests \
        pillow \
        beautifulsoup4 \
        lxml

# 拉取 Camoufox（反检测 Firefox）+ 其无头运行所需的系统库。
# `--with-deps` 调用 apt 来获取 Firefox 运行时依赖，
# 因此这必须在下面清除 apt 列表之前完成。
# 如果有错误，我们刻意让构建失败 — 一个静默损坏的浏览器
# 会生成一个看似正常的镜像，直到第一次 `camoufox-cli open`
# 在运行时爆炸，这比带有可操作错误消息的构建时失败更糟糕。
RUN camoufox-cli install --with-deps

RUN rm -rf /var/lib/apt/lists/*

# 当调用者未显式传递 --proxy 时，从 $HTTPS_PROXY（或其小写变体）
# 自动注入 --proxy 到每次 camoufox-cli 调用中。
# Playwright/Camoufox 不会为浏览器进程拾取 HTTPS_PROXY —
# 只有 --proxy 标志有效 — 因此没有这个垫片，位于纯代理网络
#（如 GFW 后）中的沙箱会在首次 Page.goto 时静默失败并显示
# `NS_ERROR_NET_INTERRUPT`，即使同一容器中的 curl / pip / npm
# 通过环境变量正常工作。
#
# 将 pip 安装的入口点移动到 camoufox-cli-bin，并将
# /usr/local/bin/camoufox-cli 替换为垫片，以便基于 PATH 的
# 查找（agent 调用它的唯一方式）透明地流经我们。
RUN mv /usr/local/bin/camoufox-cli /usr/local/bin/camoufox-cli-bin \
 && printf '%s\n' \
        '#!/bin/sh' \
        'proxy="${HTTPS_PROXY:-${https_proxy:-${HTTP_PROXY:-${http_proxy:-}}}}"' \
        'if [ -n "$proxy" ]; then' \
        '    for a in "$@"; do' \
        '        case "$a" in' \
        '            --proxy|--proxy=*) exec /usr/local/bin/camoufox-cli-bin "$@" ;;' \
        '        esac' \
        '    done' \
        '    exec /usr/local/bin/camoufox-cli-bin --proxy "$proxy" "$@"' \
        'fi' \
        'exec /usr/local/bin/camoufox-cli-bin "$@"' \
    > /usr/local/bin/camoufox-cli \
 && chmod +x /usr/local/bin/camoufox-cli

WORKDIR /workspace
