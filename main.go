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

	epay "github.com/liuscraft/epay-sdk-go"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/SakuraOpenSource/levis/pkg/plugin"
	pb "github.com/SakuraOpenSource/levis/pkg/plugin/proto"
)

const (
	pluginName    = "易支付"
	pluginVersion = "2.0.6"
	pluginDesc    = "对接易支付 V1，全量采用 MD5 验证（SDK + POST 回退，支持调试日志）"
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
	debug       bool
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
				Key:          "debug",
				Label:        "调试模式",
				Type:         pb.FieldType_FIELD_TYPE_BOOL,
				Required:     false,
				DefaultValue: "0",
				Hint:         "开启后在插件日志中输出所有请求 URL、参数与网关响应",
			},
		},
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
	p.debug = values["debug"] == "1" || strings.EqualFold(values["debug"], "true")
	if p.paymentType == "" {
		p.paymentType = "alipay"
	}
	if p.paymentType != "" && p.paymentType != "alipay" && p.paymentType != "wxpay" {
		return &pb.ConfigureReply{Error: "支付方式仅支持支付宝或微信支付"}, nil
	}
	p.notifyURL = p.apiBase + "/payment-notify/epay"

	if p.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] 易支付插件已配置: Gateway=%s Debug=ON\n", p.gatewayURL)
	} else {
		fmt.Fprintf(os.Stderr, "易支付插件已配置（全局配置已废弃，按支付方式配置）: Gateway=%s\n", p.gatewayURL)
	}
	return &pb.ConfigureReply{}, nil
}

func (p *epayPlugin) debugf(format string, args ...any) {
	if p.debug {
		fmt.Fprintf(os.Stderr, "[DEBUG] "+format+"\n", args...)
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
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
	pidStr, key, gatewayURL, paymentType := epayConfig(req.GetConfig(), p.pid, p.key, p.gatewayURL, p.paymentType)
	if pidStr == "" || key == "" {
		return nil, status.Error(codes.FailedPrecondition, "支付方式未配置 PID/KEY")
	}
	if gatewayURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "未配置易支付网关地址")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "商户 ID 必须为数字")
	}
	notifyURL := req.GetNotifyUrl()
	if notifyURL == "" {
		notifyURL = p.notifyURL
	}
	// 全部采用 V1 MD5 接口，通过官方 SDK 保证签名与参数一致
	p.debugf("CreatePayment: pid=%s gateway=%s amount=%d type=%s notify=%s subject=%s clientip=%s", pidStr, gatewayURL, req.GetAmountCents(), paymentType, notifyURL, req.GetSubject())
	client, err := epay.NewClient(&epay.Config{PID: pid, Key: key, APIBaseURL: gatewayURL, Debug: p.debug})
	if err != nil {
		p.debugf("CreatePayment NewClient 失败: %v", err)
		return nil, status.Errorf(codes.InvalidArgument, "易支付配置错误: %v", err)
	}
	resp, err := client.CreatePayment(&epay.PaymentRequest{
		Type:       paymentType,
		OutTradeNo: req.GetExternalId(),
		NotifyURL:  notifyURL,
		Name:       req.GetSubject(),
		Money:      float64(req.GetAmountCents()) / 100,
		ClientIP:   strings.TrimSpace(req.GetClientIp()),
		Device:     "pc",
		Param:      req.GetExternalId(),
	})
	p.debugf("CreatePayment SDK 响应: resp=%+v err=%v", resp, err)
	if err != nil {
		p.debugf("CreatePayment SDK 错误: %v", err)
		// 兼容仅支持 POST 的网关：SDK 走 GET /mapi.php，若网关返回“未传入任何参数”则回退到 POST
		if strings.Contains(err.Error(), "未传入") {
			p.debugf("CreatePayment 回退 POST: pid=%s gateway=%s", pidStr, gatewayURL)
			params := map[string]string{
				"pid":          pidStr,
				"out_trade_no": req.GetExternalId(),
				"notify_url":   notifyURL,
				"name":         req.GetSubject(),
				"money":        formatMoney(req.GetAmountCents()),
				"type":         paymentType,
				"device":       "pc",
				"param":        req.GetExternalId(),
			}
			if cid := strings.TrimSpace(req.GetClientIp()); cid != "" {
				params["clientip"] = cid
			}
			params["sign"] = signMD5(params, key)
			params["sign_type"] = "MD5"
			base := strings.TrimRight(gatewayURL, "/")
			p.debugf("CreatePayment POST 参数: %v", params)
			if postResp, perr := postForm(ctx, base+"/mapi.php", params); perr == nil && int(postResp.Code) == 1 {
				p.debugf("CreatePayment POST 成功: payurl=%s trade_no=%s", postResp.PayURL, postResp.TradeNo)
				payURL := postResp.PayURL
				if payURL == "" {
					payURL = postResp.QRCode
				}
				if payURL == "" {
					payURL = postResp.URLScheme
				}
				if payURL != "" {
					return &pb.CreatePaymentReply{PayUrl: payURL, GatewayRef: postResp.TradeNo}, nil
				}
			} else if perr == nil {
				p.debugf("CreatePayment POST 业务错误: code=%d msg=%s", int(postResp.Code), postResp.Msg)
				return nil, status.Errorf(codes.Internal, "易支付返回错误: %s", postResp.Msg)
			} else {
				p.debugf("CreatePayment POST 网络错误: %v", perr)
			}
		}
		return nil, status.Errorf(codes.Internal, "易支付返回错误: %v", err)
	}
	payURL := resp.PayURL
	if payURL == "" {
		payURL = resp.QRCode
	}
	if payURL == "" {
		payURL = resp.URLScheme
	}
	if payURL == "" {
		return nil, status.Errorf(codes.Internal, "易支付未返回支付地址")
	}
	return &pb.CreatePaymentReply{PayUrl: payURL, GatewayRef: resp.TradeNo}, nil
}

func (p *epayPlugin) QueryPayment(ctx context.Context, req *pb.QueryPaymentRequest) (*pb.QueryPaymentReply, error) {
	pidStr, key, gatewayURL, _ := epayConfig(req.GetConfig(), p.pid, p.key, p.gatewayURL, p.paymentType)
	if pidStr == "" || key == "" {
		return nil, status.Error(codes.FailedPrecondition, "支付方式未配置 PID/KEY")
	}
	if gatewayURL == "" {
		return nil, status.Error(codes.FailedPrecondition, "未配置易支付网关地址")
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "商户 ID 必须为数字")
	}
	p.debugf("QueryPayment: pid=%s gateway=%s out_trade_no=%s", pidStr, gatewayURL, req.GetExternalId())
	client, err := epay.NewClient(&epay.Config{PID: pid, Key: key, APIBaseURL: gatewayURL, Debug: p.debug})
	if err != nil {
		p.debugf("QueryPayment NewClient 失败: %v", err)
		return nil, status.Errorf(codes.InvalidArgument, "易支付配置错误: %v", err)
	}
	detail, err := client.QueryOrder(&epay.OrderQueryRequest{OutTradeNo: req.GetExternalId()})
	p.debugf("QueryPayment 响应: detail=%+v err=%v", detail, err)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "查询订单失败: %v", err)
	}
	state := pb.PaymentState_PAYMENT_STATE_PENDING
	if detail.Status == epay.OrderStatusPaid {
		state = pb.PaymentState_PAYMENT_STATE_PAID
	}
	paidCents := parseMoneyToCents(detail.Money)
	return &pb.QueryPaymentReply{State: state, PaidAmountCents: paidCents}, nil
}

// VerifyPaymentCallback 验证易支付异步通知的签名并归一化返回结果，全部采用 V1 MD5。
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

	// 采用官方 SDK 的 V1 MD5 校验，与 epay-sdk-go 保持一致
	pidStr, key, gatewayURL, _ := epayConfig(req.GetConfig(), p.pid, p.key, p.gatewayURL, p.paymentType)
	if key == "" {
		p.debugf("Verify: 缺少 KEY, raw=%v", raw)
		return nil, status.Error(codes.FailedPrecondition, "支付方式未配置 KEY")
	}
	p.debugf("Verify: raw=%v pid=%s gateway=%s", raw, pidStr, gatewayURL)
	pid := 0
	if pidStr != "" {
		pid, _ = strconv.Atoi(pidStr)
	} else if raw["pid"] != "" {
		pid, _ = strconv.Atoi(raw["pid"])
	}
	if pid == 0 {
		pid = 1
	}
	if gatewayURL == "" {
		gatewayURL = "https://example.com"
	}
	client, err := epay.NewClient(&epay.Config{PID: pid, Key: key, APIBaseURL: gatewayURL, Debug: p.debug})
	var notifyData *epay.NotifyData
	if err == nil {
		p.debugf("Verify: 调用 SDK VerifyNotify raw=%v", raw)
		notifyData, err = client.VerifyNotify(raw)
		p.debugf("Verify: SDK 结果 notifyData=%+v err=%v", notifyData, err)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "签名验证失败")
		}
	} else {
		p.debugf("Verify: SDK 创建失败 %v，使用本地校验", err)
		// 兜底：SDK 创建失败时使用本地 MD5 校验
		expectedSign := signCallback(raw, key)
		p.debugf("Verify: 本地计算签名 expected=%s received=%s", expectedSign, receivedSign)
		if receivedSign != expectedSign {
			return nil, status.Error(codes.Unauthenticated, "签名验证失败")
		}
		notifyData = &epay.NotifyData{
			OutTradeNo:  raw["out_trade_no"],
			TradeNo:     raw["trade_no"],
			TradeStatus: raw["trade_status"],
			Money:       raw["money"],
		}
	}
	if notifyData.OutTradeNo == "" {
		return nil, status.Error(codes.InvalidArgument, "缺少商户单号")
	}
	state := pb.PaymentState_PAYMENT_STATE_PENDING
	switch notifyData.TradeStatus {
	case epay.TradeStatusSuccess:
		state = pb.PaymentState_PAYMENT_STATE_PAID
	default:
	}
	paidCents := parseMoneyToCents(notifyData.Money)
	return &pb.VerifyPaymentCallbackReply{
		ExternalId:      notifyData.OutTradeNo,
		GatewayRef:      notifyData.TradeNo,
		State:           state,
		PaidAmountCents: paidCents,
		Currency:        "CNY",
		Message:         notifyData.TradeStatus,
	}, nil
}

// signCallback 计算易支付回调参数的 MD5 签名（保留供 SDK 创建失败时兜底）。
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
	return strings.ToLower(fmt.Sprintf("%x", h))
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
