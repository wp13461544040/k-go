#!/bin/bash

# Kiro-Go 卸载脚本

set -e

# 颜色定义
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

echo "================================"
echo "  Kiro-Go 卸载脚本"
echo "================================"
echo ""

# 检测是否为 root 用户
if [ "$EUID" -ne 0 ]; then 
    echo -e "${RED}请使用 root 权限运行此脚本${NC}"
    echo "使用命令: sudo bash uninstall.sh"
    exit 1
fi

# 确认卸载
echo -e "${YELLOW}警告: 此操作将删除 Kiro-Go 及其所有数据${NC}"
read -p "确定要继续吗？(yes/no): " CONFIRM

if [ "$CONFIRM" != "yes" ]; then
    echo "取消卸载"
    exit 0
fi

PROJECT_DIR="/opt/kiro-go"

# 检查项目目录是否存在
if [ ! -d "$PROJECT_DIR" ]; then
    echo -e "${RED}未找到 Kiro-Go 安装目录${NC}"
    exit 1
fi

# 询问是否备份数据
read -p "是否备份数据？(yes/no): " BACKUP

if [ "$BACKUP" == "yes" ]; then
    BACKUP_DIR="/root/kiro-go-backup-$(date +%Y%m%d_%H%M%S)"
    echo -e "${YELLOW}正在备份数据到 $BACKUP_DIR${NC}"
    mkdir -p "$BACKUP_DIR"
    cp -r "$PROJECT_DIR/data" "$BACKUP_DIR/" 2>/dev/null || true
    echo -e "${GREEN}✓ 数据已备份到 $BACKUP_DIR${NC}"
fi

# 停止并删除容器
echo ""
echo -e "${YELLOW}[1/4] 停止并删除 Docker 容器...${NC}"
cd "$PROJECT_DIR"
docker compose down -v 2>/dev/null || true
echo -e "${GREEN}✓ 容器已停止并删除${NC}"

# 删除 Docker 镜像
echo ""
echo -e "${YELLOW}[2/4] 删除 Docker 镜像...${NC}"
docker rmi kiro-go-kiro-go 2>/dev/null || true
docker rmi $(docker images -q kiro-go*) 2>/dev/null || true
echo -e "${GREEN}✓ 镜像已删除${NC}"

# 删除项目目录
echo ""
echo -e "${YELLOW}[3/4] 删除项目文件...${NC}"
cd /
rm -rf "$PROJECT_DIR"
echo -e "${GREEN}✓ 项目文件已删除${NC}"

# 清理 Docker（可选）
echo ""
read -p "是否清理 Docker 未使用的资源？(yes/no): " CLEAN_DOCKER

if [ "$CLEAN_DOCKER" == "yes" ]; then
    echo -e "${YELLOW}[4/4] 清理 Docker 资源...${NC}"
    docker system prune -af --volumes
    echo -e "${GREEN}✓ Docker 资源已清理${NC}"
else
    echo -e "${YELLOW}[4/4] 跳过 Docker 清理${NC}"
fi

# 完成
echo ""
echo -e "${GREEN}================================${NC}"
echo -e "${GREEN}  ✓ Kiro-Go 卸载完成！${NC}"
echo -e "${GREEN}================================${NC}"
echo ""

if [ "$BACKUP" == "yes" ]; then
    echo "数据备份位置: $BACKUP_DIR"
fi

echo ""
echo "如需重新安装，请访问："
echo "https://github.com/wp13461544040/k-go"
