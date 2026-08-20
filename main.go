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
	"strconv"
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
	pluginVersion = "2.0.3"
	pluginDesc    = "对接易支付 V1，仅支持支付宝和微信支付（支持多支付方式复用）"
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
		Config: []*pb.ConfigField{},
		PaymentConfig: []*pb.ConfigField{
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
				DefaultValue: "",
				Hint:         "易支付网关地址，如 https://pay.example.com",
			},
			{
				Key:          "payment_type",
				Label:        "支付方式",
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
	if p.paymentType != "" && p.paymentType != "alipay" && p.paymentType != "wxpay" {
		return &pb.ConfigureReply{Error: "支付方式仅支持支付宝或微信支付"}, nil
	}
	p.notifyURL = p.apiBase + "/payment-notify/epay"

	fmt.Fprintf(os.Stderr, "易支付插件已配置（全局配置已废弃，按支付方式配置）: Gateway=%s\n", p.gatewayURL)
	return &pb.ConfigureReply{}, nil
}

func (p *epayPlugin) Health(context.Context, *pb.HealthRequest) (*pb.HealthReply, error) {
	return &pb.HealthReply{Ok: true}, nil
}

func epayConfig(cfg map[string]string, fallbackPid, fallbackKey, fallbackGateway, fallbackType string) (pid, key, gatewayURL, paymentType string) {
	pid = cfg["pid"]
	if pid == "" {
		pid = fallbackPid
	}
	key = cfg["key"]
	if key == "" {
		key = fallbackKey
	}
	gatewayURL = cfg["gateway_url"]
	if gatewayURL == "" {
		gatewayURL = fallbackGateway
	}
	paymentType = cfg["payment_type"]
	if paymentType == "" {
		paymentType = fallbackType
	}
	if paymentType == "" {
		paymentType = "alipay"
	}
	return
}

func (p *epayPlugin) Shutdown(context.Context, *pb.ShutdownRequest) (*pb.ShutdownReply, error) {
	go func() {
		time.Sleep(50 * time.Millisecond)
		p.server.GracefulStop()
	}()
	return &pb.ShutdownReply{}, nil
}

func (p *epayPlugin) CreatePayment(ctx context.Context, req *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	pid, key, gatewayURL, paymentType := epayConfig(req.GetConfig(), p.pid, p.key, p.gatewayURL, p.paymentType)
	if pid == "" || key == "" {
		return nil, status.Error(codes.FailedPrecondition, "支付方式未配置 PID/KEY")
	}
	if paymentType != "alipay" && paymentType != "wxpay" {
		return nil, status.Error(codes.InvalidArgument, "支付方式仅支持支付宝或微信支付")
	}
	if gatewayURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "未配置易支付网关地址")
	}
	notifyURL := req.GetNotifyUrl()
	if notifyURL == "" {
		notifyURL = p.notifyURL
	}
	// 构造易支付 API 请求，与 epay-sdk-go 保持一致使用 GET /mapi.php
	params := map[string]string{
		"pid":          pid,
		"out_trade_no": req.GetExternalId(),
		"notify_url":   notifyURL,
		"name":         req.GetSubject(),
		"money":        formatMoney(req.GetAmountCents()),
		"sign_type":    "MD5",
	}
	// 可选参数：仅当提供时才加入，与 SDK 保持一致避免多余签名因子
	if req.GetExternalId() != "" {
		params["param"] = req.GetExternalId()
	}
	if cid := strings.TrimSpace(req.GetClientIp()); cid != "" {
		params["clientip"] = cid
	}
	if paymentType != "" {
		params["type"] = paymentType
	}
	// mapi 接口默认使用 pc 设备，SDK 中为可选，此处保留以兼容部分网关
	params["device"] = "pc"
	return p.createAPIPaymentWithKey(ctx, params, key, gatewayURL)
}

// createAPIPayment 调用 mapi.php 接口创建支付订单（兼容旧调用方，使用全局配置）
func (p *epayPlugin) createAPIPayment(ctx context.Context, params map[string]string, req *pb.CreatePaymentRequest) (*pb.CreatePaymentReply, error) {
	return p.createAPIPaymentWithKey(ctx, params, p.key, p.gatewayURL)
}

func (p *epayPlugin) createAPIPaymentWithKey(ctx context.Context, params map[string]string, key, gatewayURL string) (*pb.CreatePaymentReply, error) {
	// 签名（过滤空值与 sign/sign_type 后按 ASCII 排序拼接 + key 再 MD5）
	params["sign"] = signMD5(params, key)
	params["sign_type"] = "MD5"

	base := strings.TrimRight(gatewayURL, "/")
	// 优先使用 POST，与部分仅支持 POST 的网关保持兼容
	apiURL := base + "/mapi.php"
	resp, err := postForm(ctx, apiURL, params)
	if err == nil && int(resp.Code) == 1 {
		payURL := resp.PayURL
		if payURL == "" {
			payURL = resp.QRCode
		}
		if payURL == "" {
			payURL = resp.URLScheme
		}
		if payURL != "" {
			return &pb.CreatePaymentReply{PayUrl: payURL, GatewayRef: resp.TradeNo}, nil
		}
	}
	// POST 失败或返回非成功时，判断是否为“未传入任何参数”类提示，若是则回退到 GET
	shouldFallback := false
	if err != nil {
		shouldFallback = true
	} else if resp != nil && strings.Contains(resp.Msg, "未传入") {
		shouldFallback = true
	}
	if shouldFallback {
		getURL := base + "/mapi.php?" + buildQuery(params)
		var getResp epayAPIResponse
		if err2 := httpGet(ctx, getURL, &getResp); err2 == nil && int(getResp.Code) == 1 {
			payURL := getResp.PayURL
			if payURL == "" {
				payURL = getResp.QRCode
			}
			if payURL == "" {
				payURL = getResp.URLScheme
			}
			if payURL != "" {
				return &pb.CreatePaymentReply{PayUrl: payURL, GatewayRef: getResp.TradeNo}, nil
			}
		} else if err2 == nil {
			// GET 明确返回业务错误，优先返回 GET 的错误信息
			return nil, status.Errorf(codes.Internal, "易支付返回错误: %s", getResp.Msg)
		}
	}
	// 若未回退或回退未成功，返回原始 POST 错误
	if err != nil {
		return nil, status.Errorf(codes.Internal, "调用易支付 API 失败: %v", err)
	}
	if int(resp.Code) != 1 {
		return nil, status.Errorf(codes.Internal, "易支付返回错误: %s", resp.Msg)
	}
	// 理论上已在前面返回，此处兜底
	return nil, status.Errorf(codes.Internal, "易支付未返回支付地址")
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
	pid, key, gatewayURL, _ := epayConfig(req.GetConfig(), p.pid, p.key, p.gatewayURL, p.paymentType)
	if pid == "" || key == "" {
		return nil, status.Error(codes.FailedPrecondition, "支付方式未配置 PID/KEY")
	}
	if gatewayURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "未配置易支付网关地址")
	}

	// 调用易支付查询订单接口，与 epay-sdk-go 保持一致：仅传 pid/act/out_trade_no 并签名，不直接传 key
	params := map[string]string{
		"pid":          pid,
		"act":          "order",
		"out_trade_no": req.GetExternalId(),
	}
	// 兼容部分网关同时支持 trade_no 查询
	if req.GetGatewayRef() != "" {
		// 优先 out_trade_no，若外部已提供 trade_no 也可在签名外附加（不影响签名校验）
	}
	params["sign"] = signMD5(params, key)
	params["sign_type"] = "MD5"
	apiURL := strings.TrimRight(gatewayURL, "/") + "/api.php?" + buildQuery(params)

	var result struct {
		Code    flexInt `json:"code"`
		Msg     string  `json:"msg"`
		Status  flexInt `json:"status"`
		Money   string  `json:"money"`
		TradeNo string  `json:"trade_no"`
	}

	if err := httpGet(ctx, apiURL, &result); err != nil {
		return nil, status.Errorf(codes.Internal, "查询订单失败: %v", err)
	}

	if int(result.Code) != 1 {
		return nil, status.Errorf(codes.NotFound, "订单不存在: %s", result.Msg)
	}

	state := pb.PaymentState_PAYMENT_STATE_PENDING
	if int(result.Status) == 1 {
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

	// 用本地密钥重新计算签名（优先按支付方式配置的 KEY）
	_, key, _, _ := epayConfig(req.GetConfig(), p.pid, p.key, p.gatewayURL, p.paymentType)
	if key == "" {
		return nil, status.Error(codes.FailedPrecondition, "支付方式未配置 KEY")
	}
	expectedSign := signCallback(raw, key)
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

type flexInt int

func (f *flexInt) UnmarshalJSON(data []byte) error {
	s := strings.Trim(string(data), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		*f = 0
		return nil
	}
	*f = flexInt(v)
	return nil
}

type epayAPIResponse struct {
	Code      flexInt `json:"code"`
	Msg       string  `json:"msg"`
	TradeNo   string  `json:"trade_no"`
	PayURL    string  `json:"payurl"`
	QRCode    string  `json:"qrcode"`
	URLScheme string  `json:"urlscheme"`
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
