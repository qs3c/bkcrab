# ADR: PDF engine 选择与许可证义务

- 状态：已批准
- 批准日期：2026-07-20
- 决策：采用 `pypdfium2==5.12.1` 提供的 PDFium wheel 作为 PDF engine
- 协议标识：`engine=pypdfium2`，`engineVersion=5.12.1`
- 当前锁定 wheel 的 PDFium build：`152.0.7947.0`（build `7947`）

## 背景

多模态文档 RAG 的 PDF sidecar 需要逐页文字/对象/bbox 分析、指定页渲染和内嵌栅格图提取。设计中提到的 PyMuPDF 是 AGPL/商业双许可证，在组织没有接受相应义务或购买商业许可前不能进入分发镜像。因此选择许可证兼容的替代 engine，并通过同一 `rag-parser/v1` golden contract 验证，不让协议依赖 engine-specific 类型。

## 许可证结论与分发义务

`pypdfium2` 项目声明其自身按 `Apache-2.0 OR BSD-3-Clause` 提供；其使用的 PDFium 按 BSD-style license 提供。PDFium 二进制还包含使用其它开源许可证的第三方组件。官方要求二进制再分发同时携带 PDFium 及这些组件的许可证；wheel 为具体构建提供 `BUILD_LICENSES/` 目录。

因此本仓库必须：

1. 精确锁定 `pypdfium2==5.12.1`，不得在未复审时漂移 wheel 或 PDFium build；
2. 构建镜像时保留 wheel 安装产生的 `*.dist-info/licenses/**/BUILD_LICENSES/` 及 `LICENSES/`，并在 Docker build 中校验这些文件确实存在；
3. 发布前按实际目标平台检查 wheel 的许可证集合；wheel、目标平台或链接方式改变时重新审查（部分构建可能涉及额外 runtime 许可证）；
4. 不把本 ADR 解释为对未来 PDFium 依赖集合的永久批准。

依据：

- <https://pypdfium2.readthedocs.io/en/stable/readme.html#licensing>
- <https://pdfium.googlesource.com/pdfium/+/refs/heads/main/LICENSE>
- <https://pypi.org/project/pypdfium2/5.12.1/>

## 能力与差异

已批准的窄 `PDFEngine` adapter 只暴露协议所需能力：

- 按页提取原生文字、文字对象 bbox、内嵌图片 bbox 和固定 routing signals；
- 第一阶段 analyze 不渲染整篇页面；
- 第二阶段只按服务端页码 allowlist、固定 DPI 渲染，并尝试把内嵌图片安全重编码为 PNG；
- 所有 bbox 在 adapter 边界归一化到 0..1000，协议层不接触 PDFium handle/type。

PDFium 不提供高层“表格/代码/多栏”语义模型；v1 使用确定性、版本化的保守启发式 signals。图片提取受 PDFium 可解码能力影响，单个图片提取失败只产生 warning，不影响同页安全 render。加密/损坏 PDF、页数/像素/输出配额超限按 typed error 失败或逐页降级。

## 风险控制与回滚

- PDFium 声明为非线程安全；adapter 以进程级锁串行进入 PDFium，并在页边界检查取消/超时信号。
- 输入只来自请求隔离的临时文件；输出只写入同一 0700 请求目录；日志不记录正文、图片字节或路径。
- 页数、固定 DPI、像素、资产数/字节和 tar 输出都有硬限制。
- 若 engine 初始化、健康检查或许可证校验失败，`pdf.enabled=false`；Office capability 继续独立工作。
- 回滚方式是移除 PDF endpoint 装配与依赖并恢复 `pdf.enabled=false`，不影响 Office 协议。

## 免责声明

本 ADR 记录工程选型和已知分发义务，**不是法律意见**。组织仍应由有权限的人员按实际构建产物和发布地区完成最终合规复核。
