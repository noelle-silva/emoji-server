# emoji-server

一个简单的 Go 服务器：托管 emoji 图片/gif 资源，并提供：

- 外部访问：带 `public_key` 的固定 URL
- 管理 UI：用 `ui_key` 登录，多选上传/删除资源，支持重命名文件
- 资源存储：本地文件夹（默认 `./emojis`）
  - 文件名支持中文（但不能包含 Windows 不允许的字符，如 `< > : " / \ | ? *`）

## 运行

1. 复制配置：
   - 将 `config.example.json` 复制为 `config.json`
   - 修改 `public_key` 和 `ui_key`

2. 启动：
   - `go run ./cmd/emoji-server`

3. 打开管理界面：
   - `http://localhost:8080/admin`

## 外部访问 URL（示例）

资源文件名为 `party.gif`，则外部访问：

- `http://localhost:8080/e/<public_key>/party.gif`

（UI 中也会直接显示可复制的 URL）
