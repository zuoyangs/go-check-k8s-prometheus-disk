package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bndr/gotabulate"
)

const (
	webhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=这里填写你自己的企业微信群机器人的webhook"
)

type WeChatMessage struct {
	MsgType string `json:"msgtype"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
}

// 发送消息到企业微信
func sendToWeChat(webhookURL string, message WeChatMessage) error {
	messageBytes, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("无法序列化消息: %v", err)
	}

	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(messageBytes))
	if err != nil {
		return fmt.Errorf("发送消息失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("消息发送失败，状态码: %d", resp.StatusCode)
	}

	return nil
}

// 将字符串转换为整数
func atoi(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

// 处理错误信息
func handleError(err error, msg string) {
	if err != nil {
		fmt.Printf("%s: %v\n", msg, err)
	}
}

// 执行kubectl命令
func runKubectlCommand(config, namespace, pod, container, command, duCommand string) ([]string, error) {
	kubectlArgs := []string{"exec", pod, "-n", namespace, "-c", container, "--", "/bin/sh", "-c", command}
	cmd := exec.Command("kubectl", kubectlArgs...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", config))
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("执行kubectl命令失败: %v\n错误输出: %s", err, stderr.String())
	}

	duArgs := []string{"exec", pod, "-n", namespace, "-c", container, "--", "/bin/sh", "-c", duCommand}
	duCmd := exec.Command("kubectl", duArgs...)
	duCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", config))
	var duOut, duStderr bytes.Buffer
	duCmd.Stdout = &duOut
	duCmd.Stderr = &duStderr

	if err := duCmd.Run(); err != nil {
		return []string{config, pod, out.String(), ""}, fmt.Errorf("执行du命令失败: %v\n错误输出: %s", err, duStderr.String())
	}

	return []string{config, pod, out.String(), strings.TrimSpace(duOut.String())}, nil
}

// 获取Pod列表
func getPods(config, namespace string) ([]string, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace, "-o", "custom-columns=NAME:.metadata.name")
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", config))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("获取pod列表失败 for kubeconfig %s: %v", config, err)
	}

	re := regexp.MustCompile(`(^prometheus-k8s|^prometheus-istio)`)
	pods := strings.Split(out.String(), "\n")
	var filteredPods []string
	for _, pod := range pods {
		pod = strings.TrimSpace(pod)
		if re.MatchString(pod) {
			filteredPods = append(filteredPods, pod)
		}
	}

	return filteredPods, nil
}

// 获取磁盘使用情况
func getDiskUsage(config, pod string, wg *sync.WaitGroup, results chan<- []string) {
	defer wg.Done()

	command, duCommand := getCommands(config)
	result, err := runKubectlCommand(config, "monitoring", pod, "prometheus", command, duCommand)
	if err != nil {
		fmt.Printf("kubeconfig: %s, 获取磁盘使用情况失败: %v\n", config, err)
		results <- []string{config, pod, "", ""}
		return
	}

	fmt.Printf("%s, pod: %s, 获取磁盘使用情况\n", config, pod)

	results <- result
}

// 根据配置文件选择不同的命令
func getCommands(config string) (string, string) {
	switch config {
	case
		"/root/.kube/sys/putuo-pt-rke.yaml",
		"/root/.kube/sys/stage-rke.yaml",
		"/root/.kube/sys/z-prod-ack.yaml",
		"/root/.kube/sys/z-prod-tke.yaml":
		return "df -h | grep -w /prometheus | awk '{print $2, $3, $4, $5, $6}'", "du -sh /prometheus/ | awk '{print $1, $2}'"
	default:
		return "df -h | awk '{print $1, $2, $3, $4, $5, $6}' | grep -w /prometheus", "du -sh /prometheus/ | awk '{print $1, $2}'"
	}
}

// 过滤Pods
func filterPods(pods []string, podFilter *regexp.Regexp) []string {
	var filteredPods []string
	for _, pod := range pods {
		if podFilter.MatchString(pod) {
			filteredPods = append(filteredPods, pod)
		}
	}
	return filteredPods
}

// 并发执行获取磁盘使用情况
func processDiskUsageConcurrently(pods map[string][]string, configs []string) [][]string {
	var wg sync.WaitGroup
	resultsChan := make(chan []string, 100)
	var tableData [][]string

	for _, config := range configs {
		for _, pod := range pods[config] {
			wg.Add(1)
			go getDiskUsage(config, pod, &wg, resultsChan)
		}
	}

	wg.Wait()
	close(resultsChan)

	for result := range resultsChan {
		if len(result) > 0 && result[2] != "" && result[3] != "" {
			diskUsage := strings.Fields(result[2])
			if len(diskUsage) > 4 {
				diskUsage = diskUsage[len(diskUsage)-5:]
			}
			result[2] = strings.Join(diskUsage, " ")
			tableData = append(tableData, result)
		}
	}

	return tableData
}

// 显示表格并发送至企业微信
func displayAndSendTable(tableData [][]string, webhookURL string) {
	t := gotabulate.Create(tableData)
	t.SetHeaders([]string{"KUBECONFIG", "POD", "(Size Used Avail Use%)      df -h", "du -sh /prometheus"})
	t.SetMaxCellSize(60)
	t.SetAlign("right")
	t.SetWrapStrings(true)
	fmt.Println(t.Render("simple"))

	var resultString strings.Builder
	resultString.WriteString(t.Render("simple"))

	message := WeChatMessage{
		MsgType: "text",
		Text: struct {
			Content string `json:"content"`
		}{
			Content: resultString.String(),
		},
	}

	if err := sendToWeChat(webhookURL, message); err != nil {
		fmt.Printf("发送消息到企业微信失败: %v\n", err)
	} else {
		fmt.Println("消息已成功发送到企业微信")

	}
}

// 添加一个新的辅助函数来简化集群名称
func simplifyClusterName(path string) string {
	// 移除 /root/.kube/sys/ 前缀和 .yaml 后缀
	name := strings.TrimPrefix(path, "/root/.kube/sys/")
	name = strings.TrimSuffix(name, ".yaml")
	return name
}

func main() {
	fmt.Println("开始检查 Prometheus Pod 磁盘使用情况...")
	dir := "/root/.kube/sys"
	configs := make([]string, 0)
	pods := make(map[string][]string)

	fmt.Println("正在扫描配置文件...")
	// 首先收集所有符合条件的配置文件
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			handleError(err, "遍历目录失败")
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".yaml") {
			if strings.HasPrefix(info.Name(), "dev-") ||
				strings.HasPrefix(info.Name(), "sit-") ||
				strings.HasSuffix(info.Name(), "dev-ack.yaml") ||
				strings.HasSuffix(info.Name(), "pt-rke.yaml") ||
				strings.HasSuffix(info.Name(), "stage-rke.yaml") {
				return nil
			}
			configs = append(configs, path)
			fmt.Printf("发现配置文件: %s\n", simplifyClusterName(path))
		}
		return nil
	})
	handleError(err, "遍历目录出错")

	fmt.Printf("\n共发现 %d 个集群配置，开始获取 Pod 列表...\n", len(configs))

	// 并发获取所有集群的 pod 列表
	var wg sync.WaitGroup
	var mu sync.Mutex
	podFilter := regexp.MustCompile(`^(prometheus-k8s|prometheus-istio)`)

	for _, config := range configs {
		wg.Add(1)
		go func(cfg string) {
			defer wg.Done()
			podList, err := getPods(cfg, "monitoring")
			if err != nil {
				handleError(err, fmt.Sprintf("获取pod列表失败, config: %s", cfg))
				return
			}

			filteredPods := filterPods(podList, podFilter)
			if len(filteredPods) > 0 {
				mu.Lock()
				pods[cfg] = filteredPods
				fmt.Printf("获取到集群 %s 的 Pod 列表: %v\n", simplifyClusterName(cfg), filteredPods)
				mu.Unlock()
			}
		}(config)
	}
	wg.Wait()

	fmt.Printf("\n开始检查各个 Pod 的磁盘使用情况...\n\n")

	// 处理磁盘使用情况
	tableData := processDiskUsageConcurrently(pods, configs)

	fmt.Printf("\n所有检查完成，正在生成报告...\n\n")

	// 排序和处理表格数据
	sort.Slice(tableData, func(i, j int) bool {
		percI := regexp.MustCompile(`\d+%`).FindString(tableData[i][2])
		percJ := regexp.MustCompile(`\d+%`).FindString(tableData[j][2])
		percIInt := atoi(percI[:len(percI)-1])
		percJInt := atoi(percJ[:len(percJ)-1])
		return percIInt > percJInt
	})

	// 简化集群名称
	for i := range tableData {
		tableData[i][0] = simplifyClusterName(tableData[i][0])
	}

	// 修改表格标题并显示
	t := gotabulate.Create(tableData)
	t.SetHeaders([]string{"集群名称", "POD", "(Size Used Avail Use%)      df -h", "du -sh /prometheus"})
	t.SetMaxCellSize(60)
	t.SetAlign("right")
	t.SetWrapStrings(true)
	fmt.Println(t.Render("simple"))

	fmt.Println("\n正在发送报告到企业微信...")

	// 发送到企业微信
	var resultString strings.Builder
	resultString.WriteString(t.Render("simple"))

	message := WeChatMessage{
		MsgType: "text",
		Text: struct {
			Content string `json:"content"`
		}{
			Content: resultString.String(),
		},
	}

	if err := sendToWeChat(webhookURL, message); err != nil {
		fmt.Printf("发送消息到企业微信失败: %v\n", err)
	} else {
		fmt.Println("消息已成功发送到企业微信")
	}
}
