# Levis 易支付插件

对接易支付 V1，为 Levis 提供支付宝和微信支付。插件只接受 `alipay` 与 `wxpay` 两种支付方式。

## 功能

- 创建支付宝/微信支付订单（易支付 `mapi.php`）
- 查询订单状态
- 易支付 V1 MD5 签名
- 支付通知地址由 Levis 主程序提供

## 配置

| 配置项 | 必填 | 说明 |
| --- | --- | --- |
| `pid` | 是 | 易支付商户 ID |
| `key` | 是 | 易支付商户密钥，敏感字段 |
| `gateway_url` | 是 | 易支付网关地址 |
| `payment_type` | 是 | 只能是 `alipay`（支付宝）或 `wxpay`（微信支付） |

示例见 [config.example.yml](config.example.yml)。未填写支付方式时插件默认使用支付宝；其他值会被拒绝并保持未配置状态。

## 编译与安装

```bash
go mod tidy
./build.sh
```

构建产物名为 `plugin`。完整插件 ZIP 应包含：

```text
epay/
├── plugin
└── frontend/
    └── index.html
```

将 ZIP 上传到 Levis 管理后台的「插件管理」，然后配置 `pid`、`key`、网关和支付方式，授予 `wallet:credit` 与 `order:read` 权限，最后显式启用插件。

## 支付通知

在易支付商户后台将异步通知地址配置为：

```text
https://your-domain.com/api/plugin/v1/payment-notify/epay
```

Levis 负责校验通知签名、核对订单金额并通过插件回调完成幂等入账。重复通知不会重复增加余额。

## 签名算法

易支付 V1 MD5 签名规则：

1. 按参数名 ASCII 升序排列。
2. 排除 `sign`、`sign_type` 和空值。
3. 拼接为 `key=value&key=value`，参数值不 URL 编码。
4. 末尾追加商户密钥。
5. 计算小写 MD5。

插件提交 HTTP 表单时会按 HTTP 标准进行 URL 编码；编码只用于传输，不改变签名原文。

## 安全提示

- 不要在日志、源码或公开仓库中提交商户密钥。
- 生产环境使用 HTTPS。
- 保证 Levis 的公网回调地址可被易支付访问。
- 支付到账以前必须核对签名、订单号和金额。
