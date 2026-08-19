#!/bin/bash
# 易支付插件构建脚本

set -e

echo "开始构建易支付插件..."

# 清理旧的构建产物
rm -f plugin plugin.exe

# 构建插件（禁用 CGO，确保静态编译）
echo "正在编译..."
CGO_ENABLED=0 go build -o plugin -ldflags="-s -w" .

# 设置可执行权限
chmod +x plugin

echo "✓ 构建完成: $(pwd)/plugin"

# 显示文件信息
ls -lh plugin

# 可选：运行测试
if [ "$1" == "test" ]; then
    echo ""
    echo "运行测试..."
    go test -v
fi
