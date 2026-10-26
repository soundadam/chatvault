# chatvault

把微信（macOS 版 WeChat 4）的聊天记录合并进一个只增不减的本地 SQLite 库（vault），
再保持一组 Markdown / JSON Lines 导出目录与之同步：每个聊天每月一个文件，每次同步只重写变化的部分。

- **快**：APFS clonefile 快照 + 按库签名跳过未变化的数据库；首次 48 万条约 25 秒，之后的增量同步约 1 秒。
- **不丢**：消息只增不删。被撤回、被修改的消息，原文保留在 `messages`，新形态记入 `revisions`。
- **可整理**：给聊天起别名、放进文件夹、隐藏；导出跟着移动，旧文件自动清理。
- **数据结构可演进**：库结构按 `PRAGMA user_version` 编号迁移；解析结果是派生层，解析器升级后自动重建。详见 [docs/data-model.md](docs/data-model.md)。

## 安装

需要 macOS 12 以上和 Mac 版微信 4。

```bash
brew trust --tap soundadam/tap
brew install --cask soundadam/tap/chatvault
```

之后用 `chatvault update`（或 `brew upgrade`）更新。从源码构建：`make build`，生成 `bin/chatvault`。

## 第一次使用

只支持 **Mac 版微信 4.x**（数据在 `~/Library/Containers/com.tencent.xinWeChat/Data/Documents/xwechat_files`）。
微信 3.x 的数据 chatvault 不读，`check` 会直接说明。

**导入 key 是第一次同步之前必须做的一步**，三步走：

```bash
chatvault check                 # 看缺什么：微信账号、key、vault，每个 FAIL 下面都写了怎么办
chatvault keys import keys.json # 导入你准备好的 keys.json；会先试一遍能打开几个数据库
chatvault sync                  # 复制、解密、合并、导出
```

不知道下一步做什么时，`chatvault check`、`chatvault status` 和交互界面的空白页都会给出下一条命令。

## 导入 key

微信的数据库用 SQLCipher 加密，chatvault 从 `keys.json` 读取 key。文件格式是
`{"keys": ["<64 位 hex>", ...]}`，也接受按数据库分组的
`{"message/message_0.db": {"enc_key": "<64 位 hex>"}, ...}`：

```bash
chatvault keys import keys.json
chatvault check          # 逐个数据库检查 key 能否打开
```

不给路径时，`keys import` 依次在当前目录、`~/Downloads`、`~/Desktop`、`~/Documents`、`~` 找 `keys.json`。
导入前会校验格式，并只读地试一遍账号的数据库：一个都打不开就不导入（多半是别的账号的 key，或已过期）。
key 跟着账号数据走，不跟微信版本走：重新登录后、或 `check` 报某个新分库打不开时，重新导入一次。

## 用法

```bash
# 一次性：导入数据库 key（见上一节）
chatvault keys import keys.json

# 复制、解密、合并、解析、导出
chatvault sync

# 交互界面：浏览、过滤、改别名/文件夹、隐藏、同步
chatvault            # 在终端里直接运行即可，等同 chatvault ui
```

整理与导出：

```bash
chatvault chats 家                       # 列出聊天（编号可直接用于下面的命令）
chatvault chat set 家人群 --folder family --alias 家人
chatvault chat set 某公众号 --hide
chatvault show 家人 --month 2026-10      # 在终端里看一个月

chatvault export list
chatvault export add groups --format md --kind group --dir ~/Documents/群聊
chatvault export add plain --format txt  # 格式：md、txt（纯文本）、jsonl、json（每月一个数组）
chatvault export                         # 只重写有变化的文件
```

库默认在 `~/Documents/ChatVault`，可用 `--vault` 或 `$CHATVAULT_HOME` 指定：

```
~/Documents/ChatVault/
  vault.sqlite                  # 唯一的真相
  sources/wechat/<账号>/        # 快照、解密副本、state.json、keys.json（0600）
  export/md/<文件夹>/<聊天 ID>/2026-10.md
  export/jsonl/...              # 每个导出目标还有 chats.json 索引
```

`<聊天 ID>` 是微信的用户名，如 `12345678901@chatroom`、`wxid_…`，永远不变：群改名、改别名都不会挪动文件，
只改写文件标题。名字与目录的对应在 `chats.json`（也是 `chatvault chats` 最后一列）。只有 `<文件夹>`
跟着你设的文件夹走。交互界面的聊天页第二行就是当月导出文件的路径，按 `o` 在 Finder 里显示它
（列表里按 `o` 显示该聊天最新一个月的文件）。

时间按本机时区写入导出文件。

## 安全

密钥和解密副本只在本机，`keys.json` 与 `state.json` 权限 0600；程序不会打印密钥。不要把 vault 目录放进同步盘或仓库。
