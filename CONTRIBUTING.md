# 参与本项目

## 一句话

**欢迎提 Issue，暂不接受外部 Pull Request。**

## 为什么不收 PR

不是不欢迎，而是版权口径必须先保持单一：

1. 本项目采用 **PolyForm Noncommercial 1.0.0**（见 [LICENSE](LICENSE)），
   代码版权归项目所有者所有。
2. **一旦合并了外部代码，版权就不再单一** —— 之后再想调整授权方式、把同一份代码
   用在别的产品线里，都要回头找每一位贡献者逐个取得同意。对个人项目这是长期负担。
3. 认真审一份外部 PR 的成本也不低：能用的往往要按项目风格重写才能进主干。
   与其合进来再改，不如我们照你的思路自己实现。

如果你已经提了 PR，我们会**礼貌关闭并说明原因**，但会在 Issue 里把你的思路讨论清楚 ——
思路本身我们非常欢迎。

## 提 Issue 时请带上这些

| 类型 | 需要的信息 |
|---|---|
| **Bug** | 面板与节点的版本号、发行版与内核版本（`uname -a`）、复现步骤、期望结果与实际结果 |
| **部署问题** | `systemctl status atlmcpanel-panel`（或 `atlmcpanel-daemon`）的输出、`journalctl -u <单元名> -n 50` 的日志片段 |
| **配额不生效** | `stat -fc %T /sys/fs/cgroup`（应为 `cgroup2fs`）与内核版本 —— 内核不满足时配额会自动降级并告警，这是设计行为 |
| **功能建议** | 你想解决的**实际场景**，而不是具体实现方案。场景比方案更能帮我们判断优先级 |

**日志请先打码**：把公网 IP、域名、token、密码换成占位符再贴。
面板日志与配置里可能含有节点地址与凭据。

## 安全漏洞

**请不要在公开 Issue 里贴漏洞细节或 PoC。**

请走 GitHub 的**私有漏洞报告**：仓库 `Security` 标签页 → `Report a vulnerability`。
面板本身是直接暴露在公网上的服务，我们会尽快确认并修复，修好后一起公开说明。

## 欢迎的非代码贡献

- **文档纠错**：部署文档写错一步，比一个 bug 更劝退新人 —— 直接开 Issue 指出即可。
- **翻译**：界面 i18n 与文档英文版都在计划中，可以先开 Issue 说明愿意参与。
- **使用反馈**：告诉我你在什么规模的环境里用（几台节点、多少个实例、什么发行版）。
  这类信息对判断优先级非常有用，而且不需要你写一行代码。

## 常见问题：到底能不能商用

- **可以**：个人学习、研究、自用、开服给朋友玩；学校、社团、公益组织等非营利使用。
- **不可以**：拿本软件或其修改版**对外提供收费服务**、**随主机/硬件打包售卖**、
  **作为商业产品的一部分**发布。这类使用需要单独取得授权。
- **拿不准就问**：边界情形（例如公益服接受捐赠抵扣服务器成本）开个 Issue 说明用途，
  我们会给出明确答复。

完整条款以 [LICENSE](LICENSE) 的英文原文为准；上面的说明只是方便理解，不构成额外授权。

---

## English summary

Issues are welcome. **Pull requests are not accepted at this time**, so that copyright
ownership stays undivided. Licensed under [PolyForm Noncommercial 1.0.0](LICENSE):
personal, hobby, educational, charitable and other noncommercial use is permitted;
commercial use requires a separate license. For security reports, please use GitHub's
private vulnerability reporting instead of a public issue.
