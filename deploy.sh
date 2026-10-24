#!/bin/bash

# Kiro-Go Ubuntu 服务器一键部署脚本
# 支持 Ubuntu 18.04, 20.04, 22.04, 24.04

set -e

echo "================================"
echo "  Kiro-Go 一键部署脚本"
echo "================================"
echo ""

# 颜色定义
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

# 检测是否为 root 用户
if [ "$EUID" -ne 0 ]; then 
    echo -e "${RED}请使用 root 权限运行此脚本${NC}"
    echo "使用命令: sudo bash deploy.sh"
    exit 1
fi

# 检查系统版本
if [ -f /etc/os-release ]; then
    . /etc/os-release
    OS=$ID
    VER=$VERSION_ID
    echo -e "${GREEN}✓ 检测到系统: $PRETTY_NAME${NC}"
else
    echo -e "${RED}无法识别的操作系统${NC}"
    exit 1
fi

# 更新系统
echo ""
echo -e "${YELLOW}[1/6] 更新系统包...${NC}"
apt-get update -y
apt-get upgrade -y

# 安装必要的依赖
echo ""
echo -e "${YELLOW}[2/6] 安装必要依赖...${NC}"
apt-get install -y \
    curl \
    wget \
    git \
    ca-certificates \
    gnupg \
    lsb-release

# 安装 Docker
echo ""
echo -e "${YELLOW}[3/6] 检查并安装 Docker...${NC}"
if command -v docker &> /dev/null; then
    echo -e "${GREEN}✓ Docker 已安装: $(docker --version)${NC}"
else
    echo "正在安装 Docker..."
    
    # 添加 Docker 官方 GPG 密钥
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
    chmod a+r /etc/apt/keyrings/docker.gpg
    
    # 添加 Docker 仓库
    echo \
      "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu \
      $(lsb_release -cs) stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null
    
    # 安装 Docker Engine
    apt-get update -y
    apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    
    # 启动 Docker
    systemctl start docker
    systemctl enable docker
    
    echo -e "${GREEN}✓ Docker 安装完成: $(docker --version)${NC}"
fi

# 克隆项目
echo ""
echo -e "${YELLOW}[4/6] 克隆项目代码...${NC}"
PROJECT_DIR="/opt/kiro-go"

if [ -d "$PROJECT_DIR" ]; then
    echo -e "${YELLOW}项目目录已存在，正在备份...${NC}"
    mv "$PROJECT_DIR" "${PROJECT_DIR}_backup_$(date +%Y%m%d_%H%M%S)"
fi

git clone https://github.com/wp13461544040/k-go.git "$PROJECT_DIR"
cd "$PROJECT_DIR"

# 创建数据目录
mkdir -p data

# 配置环境变量
echo ""
echo -e "${YELLOW}[5/6] 配置环境变量...${NC}"

# 询问管理员密码
read -p "请设置管理员密码 (默认: changeme): " ADMIN_PASSWORD
ADMIN_PASSWORD=${ADMIN_PASSWORD:-changeme}

# 询问端口
read -p "请设置服务端口 (默认: 8080): " PORT
PORT=${PORT:-8080}

# 创建 .env 文件
cat > .env << EOF
# Kiro-Go 配置文件
ADMIN_PASSWORD=${ADMIN_PASSWORD}
PORT=${PORT}
CONFIG_PATH=/app/data/config.json
KIRO_SSO_CALLBACK_BIND=0.0.0.0
EOF

echo -e "${GREEN}✓ 环境变量配置完成${NC}"

# 启动服务
echo ""
echo -e "${YELLOW}[6/6] 启动 Kiro-Go 服务...${NC}"

# 修改 docker-compose.yml 以使用 .env 文件
cat > docker-compose.yml << 'EOF'
version: '3.8'

services:
  kiro-go:
    build: .
    container_name: kiro-go
    ports:
      - "${PORT:-8080}:8080"
      - "127.0.0.1:3128:3128"
    volumes:
      - ./data:/app/data
    environment:
      - CONFIG_PATH=${CONFIG_PATH:-/app/data/config.json}
      - ADMIN_PASSWORD=${ADMIN_PASSWORD:-changeme}
      - KIRO_SSO_CALLBACK_BIND=${KIRO_SSO_CALLBACK_BIND:-0.0.0.0}
    restart: unless-stopped
    healthcheck:
      test: ["CMD", "wget", "--no-verbose", "--tries=1", "--spider", "http://localhost:8080/admin"]
      interval: 30s
      timeout: 10s
      retries: 3
      start_period: 40s
EOF

# 构建并启动容器
# 检测 docker compose 命令格式
if docker compose version &> /dev/null; then
    # Docker Compose V2 (插件)
    docker compose up -d --build
elif docker-compose --version &> /dev/null; then
    # Docker Compose V1 (独立命令)
    docker-compose up -d --build
else
    echo -e "${RED}Docker Compose 未安装${NC}"
    exit 1
fi

# 等待服务启动
echo ""
echo -e "${YELLOW}等待服务启动...${NC}"
sleep 10

# 检查服务状态
COMPOSE_CMD="docker compose"
if ! docker compose version &> /dev/null; then
    COMPOSE_CMD="docker-compose"
fi

if $COMPOSE_CMD ps | grep -q "Up"; then
    echo ""
    echo -e "${GREEN}================================${NC}"
    echo -e "${GREEN}  ✓ Kiro-Go 部署成功！${NC}"
    echo -e "${GREEN}================================${NC}"
    echo ""
    echo "访问信息："
    echo -e "  管理面板: ${GREEN}http://$(hostname -I | awk '{print $1}'):${PORT}/admin${NC}"
    echo -e "  本地访问: ${GREEN}http://localhost:${PORT}/admin${NC}"
    echo ""
    echo "登录信息："
    echo -e "  用户名: ${GREEN}admin${NC}"
    echo -e "  密码: ${GREEN}${ADMIN_PASSWORD}${NC}"
    echo ""
    echo "API 端点："
    echo -e "  Claude API: ${GREEN}http://$(hostname -I | awk '{print $1}'):${PORT}/v1/messages${NC}"
    echo -e "  OpenAI API: ${GREEN}http://$(hostname -I | awk '{print $1}'):${PORT}/v1/chat/completions${NC}"
    echo ""
    echo "常用命令："
    echo "  查看日志: cd $PROJECT_DIR && $COMPOSE_CMD logs -f"
    echo "  重启服务: cd $PROJECT_DIR && $COMPOSE_CMD restart"
    echo "  停止服务: cd $PROJECT_DIR && $COMPOSE_CMD down"
    echo "  更新服务: cd $PROJECT_DIR && git pull && $COMPOSE_CMD up -d --build"
    echo ""
else
    echo -e "${RED}服务启动失败，请查看日志：${NC}"
    echo "$COMPOSE_CMD logs"
    exit 1
fi
