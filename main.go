// 易支付插件：对接易支付 V1 接口，为 Levis 提供支付能力。
//
// 功能清单：
//   - 创建支付宝或微信支付订单
//   - 查询订单状态
//   - 易支付 V1 MD5 签名
//
// 配置项：
//   - 商户 ID (pid)
//   - 商户密钥 (key)
//   - 易支付网关地址 (gateway_url)
//   - 支付方式 (payment_type：alipay/wxpay)
package main

import (
	"context"
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/SakuraOpenSource/levis/pkg/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

const (
	pluginName    = "易支付"
	pluginVersion = "1.1.0"
	pluginDesc    = "对接易支付 V1，仅支持支付宝和微信支付"
)

func main() {
	token := os.Getenv(plugin.EnvToken)
	if token == "" {
		fmt.Fprintln(os.Stderr, "缺少令牌")
		os.Exit(1)
	}
	apiBase := os.Getenv(plugin.EnvAPIBase)
	if apiBase == "" {
		fmt.Fprintln(os.Stderr, "缺少 API 基址")
		os.Exit(1)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "监听失败:", err)
		os.Exit(1)
	}

	server := grpc.NewServer(grpc.UnaryInterceptor(authInterceptor(token)))
	srv := &epayPlugin{
		server:  server,
		apiBase: apiBase,
	}
	pb.RegisterPluginServer(server, srv)

	// 握手行
	port := listener.Addr().(*net.TCPAddr).Port
	line, _ := json.Marshal(map[string]int{"port": port})
	fmt.Println(string(line))

	if err := server.Serve(listener); err != nil {
		fmt.Fprintln(os.Stderr, "服务退出:", err)
	}
}

// authInterceptor 校验每个请求携带的令牌。
func authInterceptor(token string) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context, req any,
		info *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
	) (any, error) {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "缺少令牌")
		}
		values := md.Get(plugin.MetadataToken)
		if len(values) == 0 {
			return nil, status.Error(codes.Unauthenticated, "缺少令牌")
		}
		if subtle.ConstantTimeCompare([]byte(values[0]), []byte(token)) != 1 {
			return nil, status.Error(codes.Unauthenticated, "令牌不匹配")
		}
		return handler(ctx, req)
	}
}

type epayPlugin struct {
	pb.UnimplementedPluginServer
	server  *grpc.Server
	apiBase string
	// 配置项在 Configure 时填充
	pid         string
	key         string
	gatewayURL  string
	notifyURL   string
	paymentType string
}

func (p *epayPlugin) Describe(context.Context, *pb.DescribeRequest) (*pb.Manifest, error) {
	return &pb.Manifest{
		Name:        pluginName,
		Version:     pluginVersion,
		Description: pluginDesc,
		Author:      "Levis Team",
		Capabilities: []pb.Capability{
			pb.Capability_CAPABILITY_CREATE_PAYMENT,
		},
		Config: []*pb.ConfigField{
			{
				Key:      "pid",
				Label:    "商户 ID",
				Type:     pb.FieldType_FIELD_TYPE_TEXT,
				Required: true,
				Hint:     "易支付平台分配的商户 ID",
			},
			{
				Key:      "key",
				Label:    "商户密钥",
				Type:     pb.FieldType_FIELD_TYPE_TEXT,
				Required: true,
				Secret:   true,
				Hint:     "易支付平台分配的商户密钥，用于签名",
			},
			{
				Key:          "gateway_url",
				Label:        "易支付网关地址",
				Type:         pb.FieldType_FIELD_TYPE_TEXT,
				Required:     true,
				DefaultValue: "https://dash.natriumgroup.com",
				Hint:         "易支付网关地址，默认为官方网关",
			},
			{
				Key:          "payment_type",
				Label:        "默认支付方式",
				Type:         pb.FieldType_FIELD_TYPE_SELECT,
				Required:     true,
				DefaultValue: "alipay",
				Hint:         "仅支持支付宝（alipay）和微信支付（wxpay）",
				Options: []*pb.SelectOption{
					{Value: "alipay", Label: "支付宝"},
					{Value: "wxpay", Label: "微信支付"},
				},
			},
		},
		RequiredScopes: []string{"wallet:credit", "order:read"},
	}, nil
}

func (p *epayPlugin) Configure(_ context.Context, req *pb.ConfigureRequest) (*pb.ConfigureReply, error) {
	values := req.GetValues()
	p.pid = values["pid"]
	p.key = values["key"]
	p.gatewayURL = values["gateway_url"]
	p.paymentType = values["payment_type"]
	if p.paymentType == "" {
		p.paymentType = "alipay"
	}
	if p.paymentType != "alipay" && p.paymentType != "wxpay" {
		return &pb.ConfigureReply{Error: "支付方式仅支持支付宝或微信支付"}, nil
	}
	p.notifyURL = p.apiBase + "/payment-notify/epay"

	if p.gatewayURL == "" {
		p.gatewayURL = "https://dash.natriumgroup.com"
	}

	fmt.Fprintf(os.Stderr, "易支付插件已配置: PID=%s, Gateway=%s\n", p.pid, p.gatewayURL)
	return &pb.ConfigureReply{}, nil
}

func (p *epayPlugin) Health(context.Context, *pb.HealthRequest) (*pb.HealthReply, error) {
	if p.pid == "" || p.key == "" {
		return &pb.HealthReply{Ok: false, Message: "插件未配置"}, nil
	}
	return &pb.HealthReply{Ok: true}, nil
}

func (p *epayPlugin) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownReply, error) {
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.server.GracefulStop()
	}()
	return &pb.ShutdownReply{}, nil
}

func (p *epayPlugin) CreatePayment(ctx context.Context, req *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	if p.pid == "" || p.key == "" {
		return nil, status.Error(codes.FailedPrecondition, "插件未配置")
	}

	// 构造易支付 API 请求
	params := map[string]string{
		"pid":          p.pid,
		"out_trade_no": req.GetExternalId(),
		"notify_url":   p.notifyURL,
		"name":         req.GetSubject(),
		"money":        formatMoney(req.GetAmountCents()),
		"param":        req.GetExternalId(), // 回传订单 ID
		"sign_type":    "MD5",
		"clientip":     req.GetClientIp(),
	}

	// payment_type 已在 Configure 中限制为 alipay/wxpay。
	params["type"] = p.paymentType
	params["device"] = "pc"
	return p.createAPIPayment(ctx, params, req)
}

// createAPIPayment 调用 mapi.php 接口创建支付订单
func (p *epayPlugin) createAPIPayment(ctx context.Context, params map[string]string, req *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	// 签名
	params["sign"] = signMD5(params, p.key)

	// 发起 HTTP POST 请求
	apiURL := p.gatewayURL + "/mapi.php"
	resp, err := postForm(ctx, apiURL, params)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "调用易支付 API 失败: %v", err)
	}

	if resp.Code != 1 {
		return nil, status.Errorf(codes.Internal, "易支付返回错误: %s", resp.Msg)
	}

	// 返回支付跳转 URL 或二维码
	return &pb.CreatePaymentReply{
		PayUrl:     resp.PayURL,
		GatewayRef: resp.TradeNo,
	}, nil
}

// createSubmitPayment is retained for compatibility with older callers; current configuration always uses mapi.php.
func (p *epayPlugin) createSubmitPayment(params map[string]string, req *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	params["return_url"] = p.notifyURL // 同步回调地址
	params["sign"] = signMD5(params, p.key)

	// 拼接成 URL
	submitURL := p.gatewayURL + "/submit.php?" + buildQuery(params)

	return &pb.CreatePaymentReply{
		PayUrl:     submitURL,
		GatewayRef: req.GetExternalId(), // 尚未获取到平台订单号，先用商户订单号
	}, nil
}

func (p *epayPlugin) QueryPayment(ctx context.Context, req *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	if p.pid == "" || p.key == "" {
		return nil, status.Error(codes.FailedPrecondition, "插件未配置")
	}

	// 调用易支付查询订单接口，out_trade_no 是商户单号
	apiURL := fmt.Sprintf("%s/api.php?act=order&pid=%s&key=%s&out_trade_no=%s",
		p.gatewayURL, p.pid, p.key, req.GetExternalId())

	var result struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		Status  int    `json:"status"`
		Money   string `json:"money"`
		TradeNo string `json:"trade_no"`
	}

	if err := httpGet(ctx, apiURL, &result); err != nil {
		return nil, status.Errorf(codes.Internal, "查询订单失败: %v", err)
	}

	if result.Code != 1 {
		return nil, status.Errorf(codes.NotFound, "订单不存在: %s", result.Msg)
	}

	state := pb.PaymentState_PAYMENT_STATE_PENDING
	if result.Status == 1 {
		state = pb.PaymentState_PAYMENT_STATE_PAID
	}

	paidCents := parseMoneyToCents(result.Money)

	return &pb.QueryPaymentReply{
		State:           state,
		PaidAmountCents: paidCents,
	}, nil
}

// VerifyPaymentCallback 验证易支付异步通知的签名并归一化返回结果。
func (p *epayPlugin) VerifyPaymentCallback(_ context.Context, req *pb.VerifyPaymentCallbackRequest) (*pb.VerifyPaymentCallbackReply, error) {
	raw := req.GetRaw()
	if raw == nil {
		return nil, status.Error(codes.InvalidArgument, "缺少回调参数")
	}

	// 校验收到的签名
	receivedSign := raw["sign"]
	signType := raw["sign_type"]
	if signType != "" && signType != "MD5" {
		return nil, status.Error(codes.InvalidArgument, "不支持的签名类型")
	}

	// 用本地密钥重新计算签名
	expectedSign := signCallback(raw, p.key)
	if receivedSign != expectedSign {
		return nil, status.Error(codes.Unauthenticated, "签名验证失败")
	}

	externalID := raw["out_trade_no"]
	if externalID == "" {
		return nil, status.Error(codes.InvalidArgument, "缺少商户单号")
	}

	tradeStatus := raw["trade_status"]
	gatewayRef := raw["trade_no"]
	money := raw["money"]

	state := pb.PaymentState_PAYMENT_STATE_PENDING
	switch tradeStatus {
	case "TRADE_SUCCESS":
		state = pb.PaymentState_PAYMENT_STATE_PAID
	default:
		// 非成功状态暂不处理，主程序会保持 pending
	}

	paidCents := parseMoneyToCents(money)

	return &pb.VerifyPaymentCallbackReply{
		ExternalId:      externalID,
		GatewayRef:      gatewayRef,
		State:           state,
		PaidAmountCents: paidCents,
		Currency:        "CNY",
		Message:         tradeStatus,
	}, nil
}

// signCallback 计算易支付回调参数的 MD5 签名。
func signCallback(params map[string]string, key string) string {
	var keys []string
	for k := range params {
		if k != "sign" && k != "sign_type" && params[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var pairs []string
	for _, k := range keys {
		pairs = append(pairs, k+"="+params[k])
	}
	str := strings.Join(pairs, "&") + key
	h := md5.Sum([]byte(str))
	return hex.EncodeToString(h[:])
}

// parseMoneyToCents 将元字符串（如 "1.00"）转为分。
func parseMoneyToCents(money string) int64 {
	if money == "" {
		return 0
	}
	var yuan float64
	if _, err := fmt.Sscanf(money, "%f", &yuan); err != nil {
		return 0
	}
	return int64(yuan*100 + 0.5)
}

// formatMoney 将分转换为元（保留两位小数）
func formatMoney(cents int64) string {
	yuan := float64(cents) / 100.0
	return fmt.Sprintf("%.2f", yuan)
}

// httpGet 发起 GET 请求并解析 JSON 响应
func httpGet(ctx context.Context, url string, result any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	return json.NewDecoder(resp.Body).Decode(result)
}

// postForm 发起 POST 表单请求并解析 JSON 响应
func postForm(ctx context.Context, apiURL string, params map[string]string) (*epayAPIResponse, error) {
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", apiURL, strings.NewReader(values.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result epayAPIResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

type epayAPIResponse struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	TradeNo   string `json:"trade_no"`
	PayURL    string `json:"payurl"`
	QRCode    string `json:"qrcode"`
	URLScheme string `json:"urlscheme"`
}

// buildQuery 构造 URL 查询字符串（已转义）
func buildQuery(params map[string]string) string {
	values := url.Values{}
	for k, v := range params {
		values.Set(k, v)
	}
	return values.Encode()
}

// signMD5 计算 MD5 签名
func signMD5(params map[string]string, key string) string {
	// 按键名 ASCII 排序
	var keys []string
	for k := range params {
		// sign、sign_type 和空值不参与签名
		if k != "sign" && k != "sign_type" && params[k] != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	// 拼接成 URL 键值对格式，参数值不进行 URL 编码
	var pairs []string
	for _, k := range keys {
		pairs = append(pairs, k+"="+params[k])
	}
	str := strings.Join(pairs, "&")

	// 追加商户密钥并计算 MD5
	str += key
	h := md5.New()
	io.WriteString(h, str)
	return hex.EncodeToString(h.Sum(nil))
}
