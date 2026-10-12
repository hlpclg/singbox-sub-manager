package main

// node_menu.go implements design spec §4.4's interactive menu, `sudo
// proxyctl node` with no arguments on a terminal (plan Task 10, spec §17.2).
//
// The menu is only a collector: it reads the host's role and instance state,
// asks questions, prints the equivalent full command (flags before the node
// name, single-quoted values, '' for an empty one -- spec §4.5), and after an
// explicit confirmation calls cmdNode with exactly that argv. There is no
// second write path: install/reinstall/show/uninstall go through `node
// service ...`, and add/edit/enable/disable/remove/republish go through the
// strict `--publish` / `node publish` business path of Task 9, so the usual
// closed publish loop, exit codes and error texts apply unchanged.
//
// Lock timing (spec §4.4, §11.2): the menu itself takes no lock and writes
// nothing. Reading the role, the instance state and the node list is
// read-only; every lock is taken inside the business command, after the
// user's final confirmation. EOF or a "no" at any prompt returns before
// anything was dispatched. After the confirmation, SIGINT/SIGTERM handling is
// whatever the business command installs (installBaseContext); the menu has
// no goroutines or signal handlers of its own.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/hlpclg/singbox-sub-manager/internal/nodes"
	"github.com/hlpclg/singbox-sub-manager/internal/realitynode"
	"github.com/hlpclg/singbox-sub-manager/internal/role"
)

// nodeMenuEnv is the injectable seam of the menu: terminal detection, the
// effective uid, the read-only role/state probes, the public-IP probe and the
// dispatch into the business commands. Production wires the real ones; tests
// replace newNodeMenuEnv wholesale so no terminal, root or host is needed.
type nodeMenuEnv struct {
	stdin          io.Reader
	stdinIsTTY     func() bool
	stdoutIsTTY    func(stdout io.Writer) bool
	geteuid        func() int
	detectRole     func() role.Result
	instanceState  func() realitynode.DetectResult
	publishPending func() bool
	publicIPv4     func(ctx context.Context) (string, error)
	// dispatch runs a `proxyctl node` command (args exclude the leading
	// "node"): the same entry point the command line uses.
	dispatch func(args []string, stdout, stderr io.Writer) int
}

// newNodeMenuEnv is assigned in init: productionNodeMenuEnv dispatches to
// cmdNode, which opens the menu, and a package-level initializer would be an
// initialization cycle.
var newNodeMenuEnv func() nodeMenuEnv

func init() { newNodeMenuEnv = productionNodeMenuEnv }

const (
	ipifyURL       = "https://api.ipify.org"
	publicIPv4Wait = 10 * time.Second
)

func productionNodeMenuEnv() nodeMenuEnv {
	lock := newNodeLockEnv()
	return nodeMenuEnv{
		stdin:      os.Stdin,
		stdinIsTTY: func() bool { return isTTY(os.Stdin) },
		stdoutIsTTY: func(w io.Writer) bool {
			f, ok := w.(*os.File)
			return ok && isTTY(f)
		},
		geteuid: os.Geteuid,
		detectRole: func() role.Result {
			return role.DetectSubscriptionCenter(role.OSFS(), role.ProdPaths())
		},
		instanceState: func() realitynode.DetectResult {
			return realitynode.DetectInstanceState(lock.realityFS, lock.realityRoots)
		},
		publishPending: func() bool {
			return publishAreaPresent(lock.publishTxnDir, lock.publishTombGlob)
		},
		publicIPv4: func(ctx context.Context) (string, error) {
			return fetchPublicIPv4(ctx, ipv4OnlyClient(), ipifyURL)
		},
		dispatch: cmdNode,
	}
}

// publishAreaPresent reports a leftover publish transaction area or tombstone
// (spec §7.6/§11.2), the same two traces legacyGuardReason refuses on.
func publishAreaPresent(txnDir, tombGlob string) bool {
	if _, err := os.Stat(txnDir); err == nil {
		return true
	}
	matches, _ := filepath.Glob(tombGlob)
	return len(matches) > 0
}

// ipv4OnlyClient is the equivalent of the installer's `curl -4`: IPv4 only,
// no proxy from the environment, a hard overall timeout.
func ipv4OnlyClient() *http.Client {
	d := &net.Dialer{Timeout: publicIPv4Wait}
	return &http.Client{
		Timeout: publicIPv4Wait,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				return d.DialContext(ctx, "tcp4", addr)
			},
			TLSHandshakeTimeout: publicIPv4Wait,
		},
	}
}

// fetchPublicIPv4 asks url for the caller's public address and accepts only a
// single global-unicast IPv4 literal. The result is a default the user must
// still confirm (spec §4.4); any failure just means "no default".
func fetchPublicIPv4(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(string(body))
	ip := net.ParseIP(text)
	if ip == nil || ip.To4() == nil || strings.Contains(text, ":") {
		return "", fmt.Errorf("not an IPv4 address: %q", text)
	}
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", fmt.Errorf("not a public IPv4 address: %q", text)
	}
	return ip.String(), nil
}

// cmdNodeMenu is `proxyctl node` with no arguments. Without a terminal on
// both stdin and stdout it stays the usage text with exit 2 (spec §4.4).
func cmdNodeMenu(stdout, stderr io.Writer) int {
	env := newNodeMenuEnv()
	if !env.stdinIsTTY() || !env.stdoutIsTTY(stdout) {
		return nodeUsage(stderr)
	}
	// Spec §4.4: without root the role is not detected (the token file is
	// 0600, so the result would be wrong); just say how to rerun.
	if env.geteuid() != 0 {
		fmt.Fprintln(stderr, "错误：此菜单需要 root 权限，请使用 sudo 重新运行：sudo proxyctl node")
		return 1
	}
	s := &menuSession{env: env, in: bufio.NewReader(env.stdin), out: stdout, errw: stderr}
	s.st = menuState{role: env.detectRole(), inst: env.instanceState(), publishPending: env.publishPending()}
	return s.mainMenu()
}

// --- host state shown by the menu ---

type menuState struct {
	role           role.Result
	inst           realitynode.DetectResult
	publishPending bool
}

func roleText(r role.Result) string {
	switch r.Status {
	case role.StatusYes:
		return "是"
	case role.StatusNo:
		return "否"
	}
	if len(r.Reasons) > 0 {
		return "不确定(" + strings.Join(r.Reasons, "; ") + ")"
	}
	return "不确定"
}

func instanceText(d realitynode.DetectResult) string {
	switch d.Status {
	case realitynode.StatusUnfinishedTxn:
		return "未完成事务"
	case realitynode.StatusNotInstalled:
		return "未安装"
	case realitynode.StatusInstalled:
		return "已安装"
	case realitynode.StatusUninstalledKeepIdentity:
		return "已卸载但保留身份"
	}
	if len(d.Reasons) > 0 {
		return "不一致(" + strings.Join(d.Reasons, "; ") + ")"
	}
	return "不一致"
}

// Why an item is unavailable, from the spec §4.2 / §4.3 tables. An empty
// string means "available".

func (st menuState) roleReason(want role.Status) string {
	switch st.role.Status {
	case want:
		return ""
	case role.StatusUncertain:
		if len(st.role.Reasons) > 0 {
			return "订阅中心角色不确定：" + strings.Join(st.role.Reasons, "; ")
		}
		return "订阅中心角色不确定"
	case role.StatusYes:
		return "本机是订阅中心"
	default:
		return "本机不是订阅中心"
	}
}

func (st menuState) instanceReason(allowed ...realitynode.InstanceStatus) string {
	for _, a := range allowed {
		if st.inst.Status == a {
			return ""
		}
	}
	switch st.inst.Status {
	case realitynode.StatusUnfinishedTxn:
		return "存在未完成的事务"
	case realitynode.StatusNotInstalled:
		return "未安装"
	case realitynode.StatusInstalled:
		return "已安装，请使用幂等重装"
	case realitynode.StatusUninstalledKeepIdentity:
		return "已卸载但保留身份，请使用幂等重装恢复"
	}
	if len(st.inst.Reasons) > 0 {
		return "实例状态不一致：" + strings.Join(st.inst.Reasons, "; ")
	}
	return "实例状态不一致"
}

// stateOK is the instance-state set under which reinstall/uninstall exist.
var installedOrKept = []realitynode.InstanceStatus{realitynode.StatusInstalled, realitynode.StatusUninstalledKeepIdentity}

func (st menuState) reasonInstall() string {
	if r := st.instanceReason(realitynode.StatusNotInstalled); r != "" {
		return r
	}
	return st.roleReason(role.StatusNo)
}

func (st menuState) reasonReinstall() string {
	if r := st.instanceReason(installedOrKept...); r != "" {
		return r
	}
	return st.roleReason(role.StatusNo)
}

func (st menuState) reasonUninstall() string { return st.instanceReason(installedOrKept...) }

func (st menuState) reasonCenter() string {
	if r := st.roleReason(role.StatusYes); r != "" {
		return r
	}
	if st.publishPending {
		return "存在未完成的发布事务"
	}
	return ""
}

// --- the dialogue ---

type menuSession struct {
	env  nodeMenuEnv
	in   *bufio.Reader
	out  io.Writer
	errw io.Writer
	st   menuState
}

// errMenuCancel is returned by every prompt on EOF (Ctrl-D) and by the flows
// when the user backs out; both mean "nothing was done".
var errMenuCancel = errors.New("cancelled")

func (s *menuSession) printf(format string, a ...interface{}) { fmt.Fprintf(s.out, format, a...) }

// ask prints prompt and returns the trimmed answer line. EOF with nothing
// typed is a cancellation.
func (s *menuSession) ask(prompt string) (string, error) {
	fmt.Fprint(s.out, prompt)
	line, err := s.in.ReadString('\n')
	if err != nil && line == "" {
		fmt.Fprintln(s.out)
		return "", errMenuCancel
	}
	return strings.TrimSpace(line), nil
}

// askRequired re-asks until the answer is non-empty (EOF cancels). A default
// is accepted by an empty answer.
func (s *menuSession) askRequired(label, def string) (string, error) {
	for {
		prompt := label
		if def != "" {
			prompt += " [" + def + "]"
		}
		v, err := s.ask(prompt + ": ")
		if err != nil {
			return "", err
		}
		if v == "" {
			v = def
		}
		if v != "" {
			return v, nil
		}
		s.printf("%s不能为空。\n", label)
	}
}

// askOptional returns the answer, or def when empty; the answer may stay empty.
func (s *menuSession) askOptional(label, def string) (string, error) {
	prompt := label
	if def != "" {
		prompt += " [" + def + "]"
	}
	v, err := s.ask(prompt + ": ")
	if err != nil {
		return "", err
	}
	if v == "" {
		v = def
	}
	return v, nil
}

// askYesNo returns true only for y/yes (or, with defaultYes, an empty
// answer). Anything unrecognised is "no": the safe side.
func (s *menuSession) askYesNo(question string, defaultYes bool) (bool, error) {
	hint := "[y/N]"
	if defaultYes {
		hint = "[Y/n]"
	}
	v, err := s.ask(question + " " + hint + " ")
	if err != nil {
		return false, err
	}
	switch strings.ToLower(v) {
	case "y", "yes":
		return true, nil
	case "":
		return defaultYes, nil
	}
	return false, nil
}

// askChoice reads a number in [1,max]; "0" (or q) and EOF back out.
func (s *menuSession) askChoice(prompt string, max int) (int, error) {
	for {
		v, err := s.ask(prompt)
		if err != nil {
			return 0, err
		}
		if v == "0" || strings.EqualFold(v, "q") {
			return 0, errMenuCancel
		}
		if n, convErr := strconv.Atoi(v); convErr == nil && n >= 1 && n <= max {
			return n, nil
		}
		s.printf("无效的选择，请输入 0-%d。\n", max)
	}
}

func (s *menuSession) cancelled() int {
	s.printf("已取消，未做任何修改。\n")
	return 0
}

type menuItem struct {
	n      int
	label  string
	reason string
	run    func() int
}

func (s *menuSession) items() []menuItem {
	st := s.st
	return []menuItem{
		{1, "安装 VLESS Reality 服务", st.reasonInstall(), s.installFlow},
		{2, "显示注册命令", "", s.showFlow},
		{3, "幂等重装", st.reasonReinstall(), s.reinstallFlow},
		{4, "卸载", st.reasonUninstall(), s.uninstallFlow},
		{5, "添加远程节点", st.reasonCenter(), s.addNodeFlow},
		{6, "管理节点（列表 → edit / enable / disable / remove）", st.reasonCenter(), s.manageFlow},
		{7, "重新发布订阅", st.reasonCenter(), s.republishFlow},
	}
}

func (s *menuSession) mainMenu() int {
	s.printf("sudo proxyctl node\n")
	s.printf("主机角色：订阅中心=%s；Reality 代理服务=%s\n", roleText(s.st.role), instanceText(s.st.inst))
	// Spec §4.4: an unfinished transaction is announced under the role line
	// with its recover command; the menu itself has no recovery item.
	if s.st.inst.Status == realitynode.StatusUnfinishedTxn {
		s.printf("未完成的事务，请执行：sudo proxyctl node service recover\n")
	}
	if s.st.publishPending {
		s.printf("未完成的发布事务，请执行：sudo proxyctl node publish --recover\n")
	}
	items := s.items()
	show := func(from, to int, title string) {
		s.printf("\n%s\n", title)
		for _, it := range items[from:to] {
			if it.reason != "" {
				s.printf("  %d) %s（不可用：%s）\n", it.n, it.label, it.reason)
			} else {
				s.printf("  %d) %s\n", it.n, it.label)
			}
		}
	}
	show(0, 4, "本机代理服务")
	show(4, 7, "本机订阅中心")
	s.printf("\n  0) 退出\n")

	for {
		v, err := s.ask("\n请选择 [0-7]: ")
		if err != nil {
			return 0
		}
		if v == "0" || strings.EqualFold(v, "q") {
			return 0
		}
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 1 || n > len(items) {
			s.printf("无效的选择，请输入 0-%d。\n", len(items))
			continue
		}
		it := items[n-1]
		if it.reason != "" {
			s.printf("该项不可用：%s\n", it.reason)
			continue
		}
		return it.run()
	}
}

// --- equivalent commands ---

// menuCmd is one command being assembled: args is the argv handed to
// dispatch (after "node"), words the same tokens as they are printed.
type menuCmd struct {
	args  []string
	words []string
}

func (c *menuCmd) lit(tokens ...string) *menuCmd {
	c.args = append(c.args, tokens...)
	c.words = append(c.words, tokens...)
	return c
}

// val adds a flag whose value is shell-quoted in the printed form.
func (c *menuCmd) val(flag, value string) *menuCmd {
	c.args = append(c.args, flag, value)
	c.words = append(c.words, flag, realitynode.ShellQuote(value))
	return c
}

// pos adds a trailing positional argument (a node name), quoted when printed.
func (c *menuCmd) pos(value string) *menuCmd {
	c.args = append(c.args, value)
	c.words = append(c.words, realitynode.ShellQuote(value))
	return c
}

func (c *menuCmd) display() string { return "sudo proxyctl node " + strings.Join(c.words, " ") }

// confirmRun prints the equivalent command and, after an explicit yes,
// dispatches exactly that argv. Everything before this point is unlocked and
// write-free.
func (s *menuSession) confirmRun(c *menuCmd, defaultYes bool) int {
	s.printf("\n等价命令：\n%s\n", c.display())
	ok, err := s.askYesNo("确认执行？", defaultYes)
	if err != nil || !ok {
		return s.cancelled()
	}
	return s.env.dispatch(c.args, s.out, s.errw)
}

// --- node service flows ---

func (s *menuSession) installFlow() int {
	name, err := s.askRequired("节点名称（--name）", "")
	if err != nil {
		return s.cancelled()
	}

	// The public address probe only supplies a default; the user confirms or
	// changes it (spec §4.4).
	s.printf("正在探测本机公网 IPv4（仅作默认值，请确认或修改）……\n")
	ctx, cancel := context.WithTimeout(context.Background(), publicIPv4Wait)
	ip, ipErr := s.env.publicIPv4(ctx)
	cancel()
	if ipErr != nil {
		ip = ""
		s.printf("未能探测到公网 IPv4（%v），请手动输入。\n", ipErr)
	}
	server, err := s.askRequired("服务端地址（--server）", ip)
	if err != nil {
		return s.cancelled()
	}
	portText, err := s.askRequired("监听端口（--port）", strconv.Itoa(realitynode.DefaultPort))
	if err != nil {
		return s.cancelled()
	}
	s.printf("提示：大量部署共用同一个 SNI，与云厂商的 IP 段组合在一起，可能成为可识别的特征。\n")
	sni, err := s.askRequired("伪装域名（--sni）", realitynode.DefaultSNI)
	if err != nil {
		return s.cancelled()
	}

	port, convErr := strconv.Atoi(portText)
	if convErr != nil {
		fmt.Fprintln(s.errw, "error: --port must be an integer")
		return 2
	}
	if err := realitynode.ValidateInstallParams(realitynode.InstallParams{Name: name, Server: server, Port: port, SNI: sni}); err != nil {
		fmt.Fprintln(s.errw, "error:", err)
		return 2
	}
	c := (&menuCmd{}).lit("service", "install").val("--name", name).val("--server", server).
		val("--port", strconv.Itoa(port)).val("--sni", sni)
	return s.confirmRun(c, false)
}

func (s *menuSession) showFlow() int {
	return s.confirmRun((&menuCmd{}).lit("service", "show"), true)
}

func (s *menuSession) reinstallFlow() int {
	return s.confirmRun((&menuCmd{}).lit("service", "reinstall"), false)
}

func (s *menuSession) uninstallFlow() int {
	purge := true
	if s.st.inst.Status == realitynode.StatusInstalled {
		s.printf("\n卸载方式：\n")
		s.printf("  1) 卸载并保留身份（保留 secrets/instance/manifest，之后可用幂等重装恢复）\n")
		s.printf("  2) 彻底清除（删除全部实例数据，包括私钥，不可恢复）\n")
		s.printf("  0) 返回\n")
		choice, err := s.askChoice("请选择 [0-2]: ", 2)
		if err != nil {
			return s.cancelled()
		}
		purge = choice == 2
	} else {
		s.printf("\n当前为已卸载但保留身份，只能选择彻底清除（删除全部实例数据，包括私钥，不可恢复）。\n")
	}
	c := (&menuCmd{}).lit("service", "uninstall")
	if purge {
		c.lit("--purge")
		export, err := s.askYesNo("彻底清除前是否导出备份归档（归档含私钥，需自行妥善保管）？", false)
		if err != nil {
			return s.cancelled()
		}
		if export {
			path, err := s.askRequired("归档保存路径（绝对路径，不能已存在）", "")
			if err != nil {
				return s.cancelled()
			}
			c.val("--export-backup", path)
		}
	}
	return s.confirmRun(c, false)
}

// --- subscription-center flows ---

// ensureSectioned is spec §17.2's interactive migration step. A legacy
// default nodes.conf is shown together with its backup location and only
// migrated after an explicit yes, through the unchanged `node migrate`
// command (backup .bak semantics and the default-file lock included). A
// refusal, EOF or a failed migration stops the menu action: nothing is
// registered or published afterwards.
func (s *menuSession) ensureSectioned() (code int, proceed bool) {
	_, format, err := nodes.Load(defaultNodesPath)
	if err != nil {
		fmt.Fprintln(s.errw, "error:", err)
		return 1, false
	}
	if format != nodes.FormatLegacy {
		return 0, true
	}
	s.printf("\n默认节点文件 %s 仍是旧格式，发布前必须先迁移为分节格式。\n迁移前会把当前内容备份到 %s（已存在的同名备份会被覆盖）。\n",
		defaultNodesPath, defaultNodesPath+".bak")
	migrate := (&menuCmd{}).lit("migrate")
	s.printf("\n等价命令：\n%s\n", migrate.display())
	ok, askErr := s.askYesNo("现在迁移？", false)
	if askErr != nil || !ok {
		fmt.Fprintln(s.errw, "未迁移：节点文件仍是旧格式，无法发布。需要时请执行：sudo proxyctl node migrate")
		return 1, false
	}
	if rc := s.env.dispatch(migrate.args, s.out, s.errw); rc != 0 {
		return rc, false
	}
	return 0, true
}

func (s *menuSession) addNodeFlow() int {
	if code, ok := s.ensureSectioned(); !ok {
		return code
	}
	s.printf("\n添加远程 VLESS Reality 节点（信息取自节点机上 `node service show` 的输出）。\n")
	name, err := s.askRequired("节点名称（--name）", "")
	if err != nil {
		return s.cancelled()
	}
	server, err := s.askRequired("服务端地址（--server）", "")
	if err != nil {
		return s.cancelled()
	}
	portText, err := s.askRequired("端口（--port）", strconv.Itoa(realitynode.DefaultPort))
	if err != nil {
		return s.cancelled()
	}
	sni, err := s.askRequired("SNI（--sni）", "")
	if err != nil {
		return s.cancelled()
	}
	uuid, err := s.askRequired("UUID（--uuid）", "")
	if err != nil {
		return s.cancelled()
	}
	publicKey, err := s.askRequired("Reality 公钥（--public-key）", "")
	if err != nil {
		return s.cancelled()
	}
	shortID, err := s.askOptional("short id（--short-id，可留空）", "")
	if err != nil {
		return s.cancelled()
	}

	port, convErr := strconv.Atoi(portText)
	if convErr != nil {
		fmt.Fprintln(s.errw, "error: --port must be an integer")
		return 2
	}
	if err := RejectLeadingDash(name); err != nil {
		fmt.Fprintln(s.errw, "error:", err)
		return 2
	}
	node := nodes.Node{Name: name, Type: nodes.TypeVlessReality, Server: server, Port: port, SNI: sni,
		UUID: uuid, PublicKey: publicKey, ShortID: shortID, Enabled: true}
	if err := nodes.Validate(node); err != nil {
		fmt.Fprintln(s.errw, "error:", err)
		return 1
	}
	c := (&menuCmd{}).lit("add", "--publish", "--type", "vless-reality").
		val("--server", server).val("--port", strconv.Itoa(port)).val("--uuid", uuid).
		val("--public-key", publicKey).val("--short-id", shortID).val("--sni", sni).val("--name", name)
	return s.confirmRun(c, false)
}

func (s *menuSession) republishFlow() int {
	if code, ok := s.ensureSectioned(); !ok {
		return code
	}
	return s.confirmRun((&menuCmd{}).lit("publish"), false)
}

func nodeTypeText(n nodes.Node) string {
	if n.Type == "" {
		return string(nodes.TypeHysteria2)
	}
	return string(n.Type)
}

func (s *menuSession) manageFlow() int {
	if code, ok := s.ensureSectioned(); !ok {
		return code
	}
	ns, _, err := nodes.Load(defaultNodesPath)
	if err != nil {
		fmt.Fprintln(s.errw, "error:", err)
		return 1
	}
	if len(ns) == 0 {
		s.printf("\n没有可管理的节点。\n")
		return 0
	}
	s.printf("\n节点列表：\n")
	for i, n := range ns {
		state := "启用"
		if !n.Enabled {
			state = "禁用"
		}
		s.printf("  %d) %s  %s  %s:%d  SNI=%s  %s\n", i+1, n.Name, nodeTypeText(n), n.Server, n.Port, n.SNI, state)
	}
	s.printf("  0) 返回\n")
	idx, err := s.askChoice(fmt.Sprintf("选择节点 [0-%d]: ", len(ns)), len(ns))
	if err != nil {
		return s.cancelled()
	}
	node := ns[idx-1]

	s.printf("\n对节点 %s 的操作：\n  1) 编辑（edit）\n  2) 启用（enable）\n  3) 禁用（disable）\n  4) 删除（remove）\n  0) 返回\n", node.Name)
	action, err := s.askChoice("请选择 [0-4]: ", 4)
	if err != nil {
		return s.cancelled()
	}
	switch action {
	case 1:
		return s.editFlow(node)
	case 2:
		return s.confirmRun((&menuCmd{}).lit("enable", "--publish").pos(node.Name), false)
	case 3:
		return s.confirmRun((&menuCmd{}).lit("disable", "--publish").pos(node.Name), false)
	default:
		return s.confirmRun((&menuCmd{}).lit("remove", "--publish").pos(node.Name), false)
	}
}

// editFlow asks for each editable field with its current value; an empty
// answer keeps it, so only changed fields reach the command line.
func (s *menuSession) editFlow(node nodes.Node) int {
	s.printf("\n逐项输入新值，直接回车保持不变。\n")
	updated := node
	c := (&menuCmd{}).lit("edit", "--publish")

	changed := false
	str := func(label, flag, cur string, set func(string)) error {
		v, err := s.askOptional(label+" [当前："+cur+"]", "")
		if err != nil {
			return err
		}
		if v != "" && v != cur {
			c.val(flag, v)
			set(v)
			changed = true
		}
		return nil
	}
	if err := str("新名称（--name）", "--name", node.Name, func(v string) { updated.Name = v }); err != nil {
		return s.cancelled()
	}
	if err := str("服务端地址（--server）", "--server", node.Server, func(v string) { updated.Server = v }); err != nil {
		return s.cancelled()
	}
	portAns, err := s.askOptional("端口（--port） [当前："+strconv.Itoa(node.Port)+"]", "")
	if err != nil {
		return s.cancelled()
	}
	if portAns != "" {
		p, convErr := strconv.Atoi(portAns)
		if convErr != nil {
			fmt.Fprintln(s.errw, "error: --port must be an integer")
			return 2
		}
		if p != node.Port {
			c.val("--port", strconv.Itoa(p))
			updated.Port = p
			changed = true
		}
	}
	if err := str("SNI（--sni）", "--sni", node.SNI, func(v string) { updated.SNI = v }); err != nil {
		return s.cancelled()
	}
	if node.Type == nodes.TypeVlessReality {
		if err := str("UUID（--uuid）", "--uuid", node.UUID, func(v string) { updated.UUID = v }); err != nil {
			return s.cancelled()
		}
		if err := str("Reality 公钥（--public-key）", "--public-key", node.PublicKey, func(v string) { updated.PublicKey = v }); err != nil {
			return s.cancelled()
		}
		// "-" clears the short id: an empty answer already means "keep".
		sid, err := s.askOptional("short id（--short-id，输入 - 清空） [当前："+node.ShortID+"]", "")
		if err != nil {
			return s.cancelled()
		}
		switch {
		case sid == "-" && node.ShortID != "":
			c.val("--short-id", "")
			updated.ShortID = ""
			changed = true
		case sid != "" && sid != "-" && sid != node.ShortID:
			c.val("--short-id", sid)
			updated.ShortID = sid
			changed = true
		}
	}
	if !changed {
		s.printf("没有任何修改。\n")
		return s.cancelled()
	}
	if err := RejectLeadingDash(updated.Name); err != nil {
		fmt.Fprintln(s.errw, "error:", err)
		return 2
	}
	if err := nodes.Validate(updated); err != nil {
		fmt.Fprintln(s.errw, "error:", err)
		return 1
	}
	c.pos(node.Name)
	return s.confirmRun(c, false)
}
