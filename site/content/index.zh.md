---
title: chatvault
excerpt: 把 Mac 版微信的聊天记录存进本地一个只增不减的库，按聊天、按月导出成 Markdown 和 JSON Lines，每次同步只改变化的部分。
weight: 55
presentation:
  category: 微信聊天记录 / CLI + TUI · macOS
  label: 用 Homebrew 安装
  command:
    - brew trust --tap soundadam/tap
    - brew install --cask soundadam/tap/chatvault
  note: 免费，Apache-2.0 开源。全部在本机，不上传，没有遥测。
registry:
  github: soundadam/chatvault
  homebrew: soundadam/tap/chatvault
  docs: https://github.com/soundadam/chatvault#readme
release:
  version: "0.2.0"
  license: Apache-2.0
modules:
  - type: promo
    description: 微信的聊天记录只存在一台设备上，换机、清理、对方撤回都会让它少一块。{title} 把 Mac 版微信 4 的数据库复制、解密，合并进 `~/Documents/ChatVault` 里的一个 SQLite 库，再把每个聊天每个月写成一个 Markdown 和 JSON Lines 文件，可以直接搜、直接备份、直接交给别的工具。
    line: "{title} {version} 是 {license} 开源的免费命令行工具，支持 Apple 芯片和 Intel 的 Mac，不需要注册。"
    actions:
      - label: 文档
        url: "{docs}"
      - label: GitHub
        url: "https://github.com/{github}"

  - type: snapshot
    title: 为什么做 chatvault
    body: 微信自己的备份是整机迁移，导不出能读的文件；常见的导出工具一次导完一大份，下个月再导就得从头来。{title} 反过来：库只增不减，导出跟着库走。第一次同步 48 万条消息约 25 秒，之后每次同步通常 1 秒左右，只重写有新消息的那几个月份文件。
    note: 第一次同步前要先导入数据库的 key：`chatvault keys import keys.json`，`chatvault check` 会告诉你还缺什么。只支持 Mac 版微信 4.x。解密后的副本留在本机的 vault 目录里，别把它放进网盘或仓库。
    metrics:
      - value: "~1 秒"
        label: 没有新消息时的一次同步
      - value: "0 条"
        label: 撤回后丢掉的原文：原消息保留，撤回另记一条
      - value: "4 种"
        label: 导出格式：Markdown、纯文本、JSON Lines 和 JSON
      - value: "{version}"
        label: "当前版本 · {license}"

  - type: section
    title: 撤回、改名、清理，都不会少
    index: 1/3
    lede: 一条消息第一次进库的样子永远保留。之后微信里它被撤回、被改动，新样子另存为一个版本，导出里原文还在，旁边标「已撤回」。联系人和群改名也会留下历史。微信清理掉的旧分库，库里照样有。

  - type: section
    title: 整理一次，导出一直跟着
    index: 2/3
    lede: "`chatvault chat set 家人群 --folder family --alias 家人` 给聊天起别名、放进文件夹；`--hide` 把公众号之类的藏起来。下次同步时导出文件跟着搬家，旧文件自动删掉。要多一份只有群聊的导出放到别处，`chatvault export add` 一行就行。"

  - type: section
    title: 在终端里翻聊天记录
    index: 3/3
    lede: "直接运行 `chatvault` 打开交互界面：按最近活跃排列所有聊天，`/` 过滤，回车按月翻看，`a` 改别名，`f` 设文件夹，`h` 隐藏，`s` 在后台同步。要写脚本，`sync`、`chats`、`show`、`export` 这些子命令都能单独用。"
---
