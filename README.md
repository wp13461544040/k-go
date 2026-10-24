# K-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

将 Kiro 账号转换为 OpenAI / Anthropic 兼容的 API 服务。

如果这个项目对你有帮助，请给一个 Star ⭐

## ✨ 特性

- 🔄 兼容 Anthropic `/v1/messages` 和 OpenAI `/v1/chat/completions`
- 🎯 多账号池，轮询负载均衡
- 🔐 自动 Token 刷新，SSE 流式响应
- 🎨 Web 管理面板（中文/英文）
- 📊 使用量追踪，账号导入/导出
- 🔑 支持多种认证方式：Builder ID、IAM Identity Center、SSO Token
- 🌐 支持配置出站代理（SOCKS5 / HTTP）

## 🚀 Ubuntu 服务器一键部署（推荐）

### 快速部署

在 Ubuntu 服务器上执行以下命令：

```bash
wget -O - https://raw.githubusercontent.com/wp13461544040/k-go/main/deploy.sh | sudo bash
```

或者下载后执行：

```bash
wget https://raw.githubusercontent.com/wp13461544040/k-go/main/deploy.sh
sudo bash deploy.sh
```

部署脚本会自动：
- ✅ 更新系统包
- ✅ 安装 Docker 和 Docker Compose
- ✅ 克隆项目代码到 `/opt/kiro-go`
- ✅ 配置环境变量
- ✅ 构建并启动服务

### 系统要求

- Ubuntu 18.04 / 20.04 / 22.04 / 24.04
- 最低 1GB RAM
- 最低 10GB 磁盘空间
- Root 权限

## 📦 其他部署方式

### Docker Compose 部署

```bash
# 1. 克隆项目
git clone https://github.com/wp13461544040/k-go.git
cd k-go

# 2. 创建数据目录
mkdir -p data

# 3. 启动服务
docker compose up -d

# 4. 访问管理面板
# http://your-server-ip:8080/admin
```

### Docker Run 部署

```bash
docker run -d \
  --name kiro-go \
  -p 8080:8080 \
  -p 127.0.0.1:3128:3128 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v $(pwd)/data:/app/data \
  --restart unless-stopped \
  ghcr.io/wp13461544040/k-go:latest
```

### 从源码编译

```bash
# 1. 克隆项目
git clone https://github.com/wp13461544040/k-go.git
cd k-go

# 2. 安装 Go 1.21+
wget https://go.dev/dl/go1.23.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.23.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin

# 3. 编译
go build -o kiro-go .

# 4. 运行
./kiro-go
```

## 🎮 使用方法

### 1. 访问管理面板

打开浏览器访问：`http://your-server-ip:8080/admin`

- **默认用户名**: `admin`
- **默认密码**: `changeme` (请立即修改！)

### 2. 添加账号

在管理面板中添加您的 Kiro 账号：
- Builder ID 账号
- IAM Identity Center (企业 SSO)
- 社交账号登录（Google, GitHub）

### 3. 调用 API

#### Claude API (Anthropic 格式)

```bash
curl http://your-server-ip:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -H "x-api-key: your-api-key" \
  -d '{
    "model": "claude-sonnet-4.5",
    "max_tokens": 1024,
    "messages": [
      {
        "role": "user",
        "content": "你好，介绍一下你自己"
      }
    ]
  }'
```

#### OpenAI API 格式

```bash
curl http://your-server-ip:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {
        "role": "user",
        "content": "你好，介绍一下你自己"
      }
    ]
  }'
```

## 🔧 常用命令

### 查看服务状态

```bash
cd /opt/kiro-go
docker compose ps
```

### 查看日志

```bash
cd /opt/kiro-go
docker compose logs -f
```

### 重启服务

```bash
cd /opt/kiro-go
docker compose restart
```

### 停止服务

```bash
cd /opt/kiro-go
docker compose down
```

### 更新服务

```bash
cd /opt/kiro-go
git pull
docker compose up -d --build
```

### 备份数据

```bash
cd /opt/kiro-go
tar -czf kiro-go-backup-$(date +%Y%m%d).tar.gz data/
```

### 恢复数据

```bash
cd /opt/kiro-go
tar -xzf kiro-go-backup-YYYYMMDD.tar.gz
docker compose restart
```

## 🌟 高级配置

### 修改端口

编辑 `docker-compose.yml`：

```yaml
ports:
  - "9000:8080"  # 将 9000 改为您想要的端口
```

### 配置反向代理（Nginx）

```nginx
server {
    listen 80;
    server_name your-domain.com;

    location / {
        proxy_pass http://localhost:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        
        # SSE 支持
        proxy_buffering off;
        proxy_cache off;
        proxy_set_header Connection '';
        chunked_transfer_encoding off;
    }
}
```

### 配置 HTTPS (Let's Encrypt)

```bash
# 安装 Certbot
sudo apt-get install certbot python3-certbot-nginx

# 获取证书
sudo certbot --nginx -d your-domain.com

# 自动续期
sudo certbot renew --dry-run
```

### 环境变量说明

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `ADMIN_PASSWORD` | 管理员密码 | `changeme` |
| `PORT` | 服务端口 | `8080` |
| `CONFIG_PATH` | 配置文件路径 | `/app/data/config.json` |
| `KIRO_SSO_CALLBACK_BIND` | SSO 回调绑定地址 | `0.0.0.0` |

## 🐛 故障排查

### 服务无法启动

```bash
# 查看详细日志
docker compose logs

# 检查端口占用
sudo netstat -tulpn | grep 8080

# 重新构建镜像
docker compose down
docker compose up -d --build
```

### 无法访问管理面板

1. 检查防火墙规则：
```bash
sudo ufw allow 8080/tcp
sudo ufw reload
```

2. 检查 Docker 服务：
```bash
sudo systemctl status docker
```

### 账号刷新失败

- 检查网络连接
- 验证账号凭证是否正确
- 查看日志获取详细错误信息

## 📝 更新日志

### v1.0.0 (2024-01-20)
- 🎉 首次发布
- ✅ 支持 Claude 和 OpenAI API
- ✅ Web 管理面板
- ✅ 多账号管理
- ✅ Docker 部署支持

## 📄 许可证

本项目采用 MIT 许可证 - 查看 [LICENSE](LICENSE) 文件了解详情。

## 🤝 贡献

欢迎提交 Issue 和 Pull Request！

## ⚠️ 免责声明

本项目仅供学习和研究使用，请勿用于商业用途。使用本项目所产生的一切后果由使用者自行承担。

## 📮 联系方式

- GitHub Issues: https://github.com/wp13461544040/k-go/issues

## 🌟 Star History

如果这个项目对你有帮助，请给一个 Star ⭐

[![Star History Chart](https://api.star-history.com/svg?repos=wp13461544040/k-go&type=Date)](https://star-history.com/#wp13461544040/k-go&Date)
