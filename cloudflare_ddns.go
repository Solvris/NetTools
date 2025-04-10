package main

import (
        "bytes"
        "encoding/json"
        "errors"
        "flag"
        "fmt"
        "io"
        "log"
        "net"
        "net/http"
        "os"
        "os/exec"
        "path/filepath" // Import filepath
        "regexp"
        "strings"
        "time"
)

const (
        cloudflareAPI = "https://api.cloudflare.com/client/v4"
        zonesEndpoint = cloudflareAPI + "/zones"
)

type Config struct {
        APIToken  string `json:"api_token"`
        Zone      string `json:"zone"`      // 域名
        Record    string `json:"record"`    // DNS 记录名
        IPVersion string `json:"ipversion"` // "ipv4" 或 "ipv6"
        Interface string `json:"interface"` // 网络接口名
        TTL       int    `json:"ttl"`       // DNS Time-To-Live
        Proxied   bool   `json:"proxied"`   // 是否启用 Cloudflare 代理
        // ZoneID 将在首次成功获取后自动填充并保存回配置文件
        ZoneID string `json:"zone_id,omitempty"` // Zone UUID (缓存)
        // WorkDir 指定 .lastip 缓存文件的工作目录 (可选)
        WorkDir string `json:"work_dir,omitempty"`
}

// --- IP Address Handling ---

// getInterfaceIP 获取指定接口的第一个非私有、非链接本地的公网 IP 地址
func getInterfaceIP(iface string, ipversion string) string {
        var cmd *exec.Cmd
        var ipTypePattern string
        nowStr := time.Now().Format("2006-01-02 15:04:05") // For logging

        // 优先使用 'ip' 命令
        ipCmdPath, ipErr := exec.LookPath("ip")
        ifconfigCmdPath, ifconfigErr := exec.LookPath("ifconfig")

        if ipErr == nil {
                log.Printf("[%s] ℹ️ Using 'ip' command (%s) to find IP on interface %s", nowStr, ipCmdPath, iface)
                if ipversion == "ipv6" {
                        cmd = exec.Command(ipCmdPath, "-6", "addr", "show", iface, "scope", "global")
                        ipTypePattern = `inet6\s+([0-9a-fA-F:]+)/`
                } else {
                        cmd = exec.Command(ipCmdPath, "addr", "show", iface, "scope", "global")
                        ipTypePattern = `inet\s+([0-9.]+)/`
                }
        } else if ifconfigErr == nil {
                // 回退到 'ifconfig'
                log.Printf("[%s] ⚠️ 'ip' command not found, falling back to 'ifconfig' (%s). IP filtering might be less reliable.",
nowStr, ifconfigCmdPath, ifconfigCmdPath)
                cmd = exec.Command(ifconfigCmdPath, iface)
                if ipversion == "ipv6" {
                        ipTypePattern = `inet6\s(?:addr:\s*)?([0-9a-fA-F:]+)(?:\s|/|%)`
                } else {
                        ipTypePattern = `inet\s(?:addr:\s*)?([0-9.]+)\s`
                }
        } else {
                log.Fatalf("[%s] ❌ Neither 'ip' nor 'ifconfig' command found in PATH. Cannot get interface IP.", nowStr)
        }

        output, err := cmd.CombinedOutput()
        if err != nil {
                // If 'ip' with scope global fails, try without scope (might need manual filtering later)
                if ipErr == nil && (strings.Contains(err.Error(), "scope global") || strings.Contains(string(output), "does not support") || (err != nil && strings.Contains(err.Error(), "exit status"))) {
                        log.Printf("[%s] ⚠️ Failed to get global scope IP for %s (or command failed), trying without scope filter.",, nowStr, iface)
                        if ipversion == "ipv6" {
                                cmd = exec.Command(ipCmdPath, "-6", "addr", "show", iface)
                        } else {
                                cmd = exec.Command(ipCmdPath, "addr", "show", iface)
                        }
                        output, err = cmd.CombinedOutput() // Retry without scope
                        if err != nil {
                                log.Fatalf("[%s] ❌ Failed to get IP for interface %s (even without scope): %v\nOutput:\n%s", nowStr, iface, err, string(output))
                        }
                } else {
                        // Handle error from ifconfig or non-scope-related ip error
                        log.Fatalf("[%s] ❌ Failed to execute command for interface %s: %v\nOutput:\n%s", nowStr, iface, err, string(output))
                }
        }

        re := regexp.MustCompile(ipTypePattern)
        matches := re.FindAllStringSubmatch(string(output), -1)

        for _, match := range matches {
                if len(match) >= 2 {
                        ipStr := match[1]
                        // Final check for private/local IP, crucial if scope global wasn't used or ifconfig returned unwanted IPs
                        if !isPrivateOrLocalIP(ipStr) {
                                log.Printf("[%s] ✅ Found public IP %s on interface %s", nowStr, ipStr, iface)
                                return ipStr
                        } else {
                                log.Printf("[%s] ℹ️ Skipping private/local IP %s on interface %s", nowStr, ipStr, iface)
                        }
                }
        }

        log.Fatalf("[%s] ❌ No usable public IP address found for interface %s and IP version %s in command output:\n%s", nowStr, iface, ipversion, string(output))
        return "" // Should not be reached
}

// isPrivateOrLocalIP 判断 IP 是否为私有、回环或链接本地地址
func isPrivateOrLocalIP(ipStr string) bool {
        ip := net.ParseIP(ipStr)
        if ip == nil {
                log.Printf("[%s] ⚠️ Could not parse IP string during check: %s", time.Now().Format("2006-01-02 15:04:05"), ipStr)
                return true // Treat unparseable as potentially problematic
        }
        return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || isULA(ip)
}

// isULA 检查 IPv6 是否为 Unique Local Address (fc00::/7)
func isULA(ip net.IP) bool {
        return ip.To4() == nil && len(ip) == net.IPv6len && (ip[0] == 0xfc || ip[0] == 0xfd)
}

// --- Cloudflare API Interaction ---

// cfRequest 是执行 Cloudflare API 请求的辅助函数
func cfRequest(method, urlStr string, apiToken string, payload io.Reader) (*http.Response, []byte, error) {
        req, err := http.NewRequest(method, urlStr, payload)
        if err != nil {
                return nil, nil, fmt.Errorf("creating request failed: %w", err)
        }
        req.Header.Set("Authorization", "Bearer "+apiToken)
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("Accept", "application/json")

        client := &http.Client{Timeout: 20 * time.Second} // Increased timeout slightly
        resp, err := client.Do(req)
        if err != nil {
                return nil, nil, fmt.Errorf("request failed: %w", err)
        }
        // Defer closing the body right after checking for response error
        defer resp.Body.Close()

        body, readErr := io.ReadAll(resp.Body)
        // Return the response even if body reading fails, but prioritize readErr
        if readErr != nil {
                // Including status code in the error message can be helpful
                return resp, body, fmt.Errorf("reading response body failed (status: %s): %w", resp.Status, readErr)
        }

        return resp, body, nil
}

// getZoneID 通过 API 获取 Zone ID
func getZoneID(apiToken, zoneName string) (string, error) {
        url := fmt.Sprintf("%s?name=%s", zonesEndpoint, zoneName)
        nowStr := time.Now().Format("2006-01-02 15:04:05")
        log.Printf("[%s] ℹ️ Fetching Zone ID from Cloudflare API for zone: %s", nowStr, zoneName)

        _, body, err := cfRequest("GET", url, apiToken, nil)
        if err != nil {
                return "", fmt.Errorf("[%s] ❌ Requesting Zone ID failed: %w", nowStr, err)
        }

        var result struct {
                Success bool `json:"success"`
                Result  []struct {
                        ID string `json:"id"`
                } `json:"result"`
                Errors []interface{} `json:"errors"`
        }

        if err := json.Unmarshal(body, &result); err != nil {
                return "", fmt.Errorf("[%s] ❌ Failed to parse Zone ID response: %v\nResponse: %s", nowStr, err, string(body))
        }

        if !result.Success || len(result.Result) == 0 {
                errorMsg := "Unknown error"
                if len(result.Errors) > 0 {
                        errorBytes, _ := json.MarshalIndent(result.Errors, "", "  ")
                        errorMsg = string(errorBytes)
                }
                return "", fmt.Errorf("[%s] ❌ Could not find Zone ID for '%s'. API Success: %t. Errors:\n%s\nFull Response:\n%s",
                        nowStr, zoneName, result.Success, errorMsg, string(body))
        }

        log.Printf("[%s] ✅ Fetched Zone ID via API: %s", nowStr, result.Result[0].ID)
        return result.Result[0].ID, nil
}

// DNSRecord 表示 Cloudflare DNS 记录的关键字段
type DNSRecord struct {
        ID      string `json:"id"`
        Type    string `json:"type"`
        Name    string `json:"name"`
        Content string `json:"content"`
        Proxied bool   `json:"proxied"`
        TTL     int    `json:"ttl"`
}

// getDNSRecord 获取指定名称和类型的 DNS 记录信息
// fqdn should be the fully qualified domain name (e.g., sub.example.com or example.com for root)
func getDNSRecord(apiToken, zoneID, fqdn, recordType string) (*DNSRecord, error) {
        nowStr := time.Now().Format("2006-01-02 15:04:05")
        url := fmt.Sprintf("%s/%s/dns_records?type=%s&name=%s", zonesEndpoint, zoneID, recordType, fqdn)
        _, body, err := cfRequest("GET", url, apiToken, nil)
        if err != nil {
                return nil, fmt.Errorf("[%s] ❌ Requesting DNS record %s (%s) failed: %w", nowStr, fqdn, recordType, err)
        }

        var result struct {
                Success bool        `json:"success"`
                Result  []DNSRecord `json:"result"`
                Errors  []interface{} `json:"errors"`
        }

        if err := json.Unmarshal(body, &result); err != nil {
                return nil, fmt.Errorf("[%s] ❌ Failed to parse DNS record response for %s (%s): %w\nResponse: %s", nowStr, fqdn, recordType, err, string(body))
        }

        if !result.Success {
                errorMsg := "Unknown API error"
                if len(result.Errors) > 0 {
                        errorBytes, _ := json.MarshalIndent(result.Errors, "", "  ")
                        errorMsg = string(errorBytes)
                }
                return nil, fmt.Errorf("[%s] ❌ API error finding DNS record %s (%s): %s\nResponse: %s", nowStr, fqdn, recordType, errorMsg, string(body))
        }

        if len(result.Result) == 0 {
                log.Printf("[%s] ℹ️ No existing %s record found for %s via API.", nowStr, recordType, fqdn)
                return nil, nil // Record not found, not an error
        }

        log.Printf("[%s] ℹ️ Found existing %s record for %s via API (ID: %s, IP: %s).", nowStr, recordType, fqdn, result.Result[0].IID, result.Result[0].Content)
        if len(result.Result) > 1 {
                log.Printf("[%s] ⚠️ Warning: Found multiple %s records for %s. Using the first one (ID: %s).",
                        nowStr, recordType, fqdn, result.Result[0].ID)
        }

        return &result.Result[0], nil
}

// upsertDNSRecord 创建或更新 DNS 记录 (返回 bool 表示是否成功，以便缓存 IP)
func upsertDNSRecord(config Config, currentIP string, zoneID string) bool {
        recordType := "A"
        if config.IPVersion == "ipv6" {
                recordType = "AAAA"
        }

        var fqdn string
        if config.Record == "@" || config.Record == config.Zone { // Handle both "@" and zone name itself for root
                fqdn = config.Zone
        } else {
                fqdn = fmt.Sprintf("%s.%s", config.Record, config.Zone)
        }

        nowStr := time.Now().Format("2006-01-02 15:04:05")
        log.Printf("[%s] ℹ️ Checking DNS record %s (%s) via Cloudflare API...", nowStr, fqdn, recordType)

        existingRecord, err := getDNSRecord(config.APIToken, zoneID, fqdn, recordType)
        if err != nil {
                // getDNSRecord now includes timestamp and logs internally, just log the failure reason here
                log.Printf("[%s] ❌ Failed to check existing DNS record state: %v", nowStr, err)
                return false // Indicate failure
        }

        payload := map[string]interface{}{
                "type":    recordType,
                "name":    fqdn,
                "content": currentIP,
                "ttl":     config.TTL,
                "proxied": config.Proxied,
        }
        jsonData, err := json.Marshal(payload)
        if err != nil {
                log.Printf("[%s] ❌ Failed to marshal request data for %s: %v", nowStr, fqdn, err)
                return false // Indicate failure
        }

        action := "" // To track if we are creating or updating
        var apiErr error
        var resp *http.Response
        var body []byte

        if existingRecord != nil {
                // Record exists
                if existingRecord.Content == currentIP && existingRecord.Proxied == config.Proxied && existingRecord.TTL == config.TTL {
                        log.Printf("[%s] ✅ DNS record %s (%s) is already up-to-date (%s). No change needed.", nowStr, fqdn, recordType, currentIP)
                        return true // Indicate success (state matches)
                }
                // Update existing record
                action = "update"
                log.Printf("[%s] ℹ️ Existing record IP (%s) / settings differ from current IP (%s) / settings. Updating record ID %ss...", nowStr, existingRecord.Content, currentIP, existingRecord.ID)
                url := fmt.Sprintf("%s/%s/dns_records/%s", zonesEndpoint, zoneID, existingRecord.ID)
                resp, body, apiErr = cfRequest("PUT", url, config.APIToken, bytes.NewBuffer(jsonData))

        } else {
                // Record does not exist, create it
                action = "create"
                log.Printf("[%s] ℹ️ No existing %s record found for %s. Creating new record...", nowStr, recordType, fqdn)
                url := fmt.Sprintf("%s/%s/dns_records", zonesEndpoint, zoneID)
                resp, body, apiErr = cfRequest("POST", url, config.APIToken, bytes.NewBuffer(jsonData))
        }

        // Handle response统一处理创建或更新的响应
        return handleAPIResponse(resp, body, apiErr, fqdn, recordType, currentIP, action)
}

// handleAPIResponse processes the response (返回 bool 表示 API 操作是否成功)
func handleAPIResponse(resp *http.Response, body []byte, err error, fqdn, recordType, ip, action string) bool {
        nowStr := time.Now().Format("2006-01-02 15:04:05")
        if err != nil {
                // Log error from cfRequest (e.g., network error, timeout)
                log.Printf("[%s] ❌ Failed to %s DNS record %s (%s) - Request Error: %v", nowStr, action, fqdn, recordType, err)
                return false // Indicate failure
        }

        // Proceed to parse API response body
        var result struct {
                Success bool          `json:"success"`
                Errors  []interface{} `json:"errors"`
                Result  DNSRecord     `json:"result"` // Capture result details
        }

        // Check if body is nil before attempting unmarshal (could happen if cfRequest had read error)
        if body != nil {
                if jsonErr := json.Unmarshal(body, &result); jsonErr != nil {
                        log.Printf("[%s] ⚠️ Failed to parse API response body during %s: %v\nRaw Body: %s", nowStr, action, jsonErr,, string(body))
                        // Continue to check status code, but likely a failure if body is unparseable JSON
                }
        } else {
                log.Printf("[%s] ⚠️ API response body was nil during %s check (likely due to previous read error). Status: %s", nowSStr, action, resp.Status)
        }

        if resp.StatusCode >= 200 && resp.StatusCode < 300 && result.Success {
                actionVerb := "created"
                if action == "update" {
                        actionVerb = "updated"
                }
                // Log details from the actual result returned by the API for confirmation
                log.Printf("[%s] ✅ Successfully %s DNS record %s (%s) => %s (ID: %s, Proxied: %t, TTL: %d)",
                        nowStr, actionVerb, fqdn, recordType, result.Result.Content, result.Result.ID, result.Result.Proxied, result.Result.TTL)
                return true // Indicate success
        }

        // --- Failure Case ---
        errorMsg := fmt.Sprintf("API Status: %s", resp.Status)
        if !result.Success && len(result.Errors) > 0 {
                errorBytes, _ := json.MarshalIndent(result.Errors, "", "  ")
                errorMsg = fmt.Sprintf("%s\nAPI Errors:\n%s", errorMsg, string(errorBytes))
        } else if !result.Success {
                errorMsg = fmt.Sprintf("%s (API success=false, no specific errors returned in expected format)", errorMsg)
        } else {
                // Success was true, but status code wasn't 2xx? Should be rare.
                errorMsg = fmt.Sprintf("%s (API success=true, but status code indicates error)", errorMsg)
        }

        log.Printf("[%s] ❌ Failed to %s DNS record %s (%s).\n%s\nFull Response:\n%s",
                nowStr, action, fqdn, recordType, errorMsg, string(body))
        return false // Indicate failure
}

// --- Configuration Handling ---

// readConfig 读取 JSON 配置文件
func readConfig(path string) (Config, error) {
        nowStr := time.Now().Format("2006-01-02 15:04:05") // For logging
        file, err := os.Open(path)
        if err != nil {
                return Config{}, fmt.Errorf("opening config file '%s' failed: %w", path, err)
        }
        defer file.Close()

        var config Config
        decoder := json.NewDecoder(file)
        decoder.DisallowUnknownFields() // Catch extraneous fields in config
        err = decoder.Decode(&config)
        if err != nil {
                return Config{}, fmt.Errorf("parsing config file '%s' failed: %w", path, err)
        }

        // Basic validation
        if config.APIToken == "" {
                return Config{}, fmt.Errorf("config file '%s' is missing required field 'api_token'", path)
        }
        if config.Zone == "" {
                return Config{}, fmt.Errorf("config file '%s' is missing required field 'zone'", path)
        }
        if config.Record == "" {
                return Config{}, fmt.Errorf("config file '%s' is missing required field 'record'", path)
        }
        if config.Interface == "" {
                return Config{}, fmt.Errorf("config file '%s' is missing required field 'interface'", path)
        }
        if config.IPVersion != "ipv4" && config.IPVersion != "ipv6" {
                return Config{}, fmt.Errorf("config file '%s': invalid 'ipversion' ('%s'), must be 'ipv4' or 'ipv6'", path, config.IPVersion)
        }
        if config.TTL < 1 { // TTL 1 means 'automatic' for Cloudflare
                log.Printf("[%s] ⚠️ TTL value (%d) in config is less than 1, defaulting to 1 (automatic)", nowStr, config.TTL)
                config.TTL = 1
        }
        // Trim whitespace from WorkDir just in case
        config.WorkDir = strings.TrimSpace(config.WorkDir)

        log.Printf("[%s] ✅ Configuration loaded successfully from %s", nowStr, path)
        return config, nil
}

// writeConfig 将包含 ZoneID 的配置写回文件
func writeConfig(path string, config Config) error {
        nowStr := time.Now().Format("2006-01-02 15:04:05")
        log.Printf("[%s] ℹ️ Saving updated configuration (with Zone ID: %s) to file: %s", nowStr, config.ZoneID, path)

        jsonData, err := json.MarshalIndent(config, "", "  ") // Indent for readability
        if err != nil {
                return fmt.Errorf("failed to marshal config for writing: %w", err)
        }

        // Write with appropriate permissions (0600 recommended as it contains API key)
        err = os.WriteFile(path, jsonData, 0600)
        if err != nil {
                return fmt.Errorf("failed to write updated config file '%s': %w", path, err)
        }
        log.Printf("[%s] ✅ Successfully saved updated configuration.", nowStr)
        return nil
}

// --- IP Caching ---

// getCacheFilePath generates the path for the last IP cache file.
// It considers the optional WorkDir from the config.
func getCacheFilePath(config Config, configPath string) string {
        cacheFileName := filepath.Base(configPath) + ".lastip" // e.g., "myconfig.json.lastip"
        nowStr := time.Now().Format("2006-01-02 15:04:05")     // For logging

        if config.WorkDir != "" {
                // If WorkDir is specified, place cache file inside it
                absWorkDir, err := filepath.Abs(config.WorkDir) // Resolve to absolute path for clarity
                if err != nil {
                        log.Printf("[%s] ⚠️ Warning: Could not determine absolute path for work_dir '%s': %v. Using relative path.",
                                nowStr, config.WorkDir, err)
                        absWorkDir = config.WorkDir // Fallback
                }
                cachePath := filepath.Join(absWorkDir, cacheFileName)
                log.Printf("[%s] ℹ️ Using specified work_dir: %s. Cache file path determined as: %s",
                        nowStr, absWorkDir, cachePath)
                return cachePath
        } else {
                // Default: Place cache file next to the config file
                cachePath := configPath + ".lastip"
                log.Printf("[%s] ℹ️ No work_dir specified. Default cache file path: %s",
                        nowStr, cachePath)
                return cachePath
        }
}

// readLastIP reads the last known IP from the cache file
func readLastIP(cachePath string) (string, error) {
        nowStr := time.Now().Format("2006-01-02 15:04:05")
        content, err := os.ReadFile(cachePath)
        if err != nil {
                if errors.Is(err, os.ErrNotExist) {
                        log.Printf("[%s] ℹ️ IP cache file '%s' not found (first run or cache cleared).", nowStr, cachePath)
                        return "", nil // Not an error, just no previous IP
                }
                // Return error for other read issues (permissions, etc.)
                return "", fmt.Errorf("failed to read IP cache file '%s': %w", cachePath, err)
        }
        ip := strings.TrimSpace(string(content))
        if ip == "" {
                log.Printf("[%s] ⚠️ IP cache file '%s' exists but is empty.", nowStr, cachePath)
                return "", nil // Treat empty file same as non-existent
        }
        log.Printf("[%s] ℹ️ Read last known IP '%s' from cache '%s'", nowStr, ip, cachePath)
        return ip, nil
}

// writeLastIP writes the current IP to the cache file
func writeLastIP(cachePath string, ip string) error {
        nowStr := time.Now().Format("2006-01-02 15:04:05")
        log.Printf("[%s] ℹ️ Writing current IP '%s' to cache file '%s'", nowStr, ip, cachePath)

        // Ensure the directory exists before writing the file if it's not the default location.
        cacheDir := filepath.Dir(cachePath)
        // Check if the directory needs creation (simple check: is it "." or "/"?)
        if cacheDir != "." && cacheDir != "/" {
                if _, err := os.Stat(cacheDir); os.IsNotExist(err) {
                        log.Printf("[%s] ℹ️ Cache directory '%s' does not exist, attempting to create.", nowStr, cacheDir)
                        // Use 0750 for directory permissions (owner rwx, group rx, others ---)
                        // Adjust if different permissions are needed.
                        if mkdirErr := os.MkdirAll(cacheDir, 0750); mkdirErr != nil {
                                return fmt.Errorf("failed to create cache directory '%s': %w", cacheDir, mkdirErr)
                        }
                        log.Printf("[%s] ✅ Successfully created cache directory '%s'.", nowStr, cacheDir)
                }
        }

        // Write with restrictive permissions (0600: owner rw, group ---, others ---)
        err := os.WriteFile(cachePath, []byte(ip+"\n"), 0600)
        if err != nil {
                return fmt.Errorf("failed to write IP cache file '%s': %w", cachePath, err)
        }
        log.Printf("[%s] ✅ Successfully wrote IP to cache file.", nowStr)
        return nil
}

// --- Main Execution ---

func main() {
        log.SetFlags(0) // Use custom timestamp format
        nowStr := time.Now().Format("2006-01-02 15:04:05") // For initial logs

        // --- 0. Parse Command Line Arguments ---
        configFile := flag.String("f", "", "Path to config JSON file (required)")
        flag.Parse()

        if *configFile == "" {
                fmt.Fprintf(os.Stderr, "[%s] ❌ Error: Configuration file path is required.\n", nowStr)
                fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
                flag.PrintDefaults()
                os.Exit(1)
        }
        // Get absolute path for config file for consistency in logging and cache path generation
        absConfigFile, err := filepath.Abs(*configFile)
        if err != nil {
                log.Printf("[%s] ⚠️ Warning: Could not determine absolute path for config file '%s': %v. Using provided path.", nowSStr, *configFile, err)
                absConfigFile = *configFile // Fallback
        }


        log.Printf("[%s] ========= Starting Cloudflare DDNS Update =========", nowStr)
        log.Printf("[%s] Using configuration file: %s", nowStr, absConfigFile)

        // --- 1. Read Configuration ---
        config, err := readConfig(absConfigFile)
        if err != nil {
                log.Fatalf("[%s] ❌ Error loading configuration: %v", time.Now().Format("2006-01-02 15:04:05"), err)
        }

        // --- 2. Get Current IP ---
        currentIP := getInterfaceIP(config.Interface, config.IPVersion) // This will Fatalf if it fails

        // --- 3. Check IP Cache ---
        cacheFilePath := getCacheFilePath(config, absConfigFile) // Use abs path
        lastIP, err := readLastIP(cacheFilePath)
        if err != nil {
                // Log non-critical read error but continue (will force API check)
                log.Printf("[%s] ⚠️ Warning: Could not read last IP cache '%s': %v", time.Now().Format("2006-01-02 15:04:05"), cacheeFilePath, err)
        }

        if currentIP == lastIP && lastIP != "" { // Ensure lastIP is not empty
                log.Printf("[%s] ✅ Current IP (%s) matches cached IP from '%s'. No update needed.", time.Now().Format("2006-01-02 15:04:05"), currentIP, cacheFilePath)
                log.Printf("[%s] ========= Cloudflare DDNS Update Completed (No Action) =========", time.Now().Format("2006-01-02 15:04:05"))
                os.Exit(0) // Exit successfully
        } else if lastIP != "" {
                log.Printf("[%s] ℹ️ Current IP (%s) differs from cached IP (%s). Proceeding with Cloudflare check.", time.Now().Formmat("2006-01-02 15:04:05"), currentIP, lastIP)
        } else {
         log.Printf("[%s] ℹ️ No valid cached IP found. Proceeding with Cloudflare check.", time.Now().Format("2006-01-02 15:04:05"))
    }


        // --- 4. Handle Zone ID (Cache or Fetch) ---
        zoneID := config.ZoneID
        if zoneID == "" {
                fetchedZoneID, err := getZoneID(config.APIToken, config.Zone)
                if err != nil {
                        // Fatal if we can't get the Zone ID when needed
                        log.Fatalf("[%s] ❌ Error fetching Zone ID: %v", time.Now().Format("2006-01-02 15:04:05"), err)
                }
                config.ZoneID = fetchedZoneID // Update in memory
                zoneID = fetchedZoneID

                // Attempt to save the updated config with the Zone ID
                // Use the absolute config file path for writing
                if writeErr := writeConfig(absConfigFile, config); writeErr != nil {
                        // Log failure to write but continue the current run with the fetched ID
                        log.Printf("[%s] ⚠️ Warning: Failed to save Zone ID to config file '%s': %v", time.Now().Format("2006-01-02
15:04:05"), absConfigFile, writeErr)
                        log.Printf("[%s] ℹ️ Will continue this run using the fetched Zone ID, but it won't be cached for next time uunless manually added or file permissions fixed.", time.Now().Format("2006-01-02 15:04:05"))
                }
        } else {
                log.Printf("[%s] ✅ Using cached Zone ID from config file: %s", time.Now().Format("2006-01-02 15:04:05"), zoneID)
        }

        // --- 5. Upsert DNS Record ---
        // upsertDNSRecord now returns true on success (including "no change needed"), false on failure
        success := upsertDNSRecord(config, currentIP, zoneID)

        // --- 6. Update IP Cache on Success ---
        if success {
                // Use the cacheFilePath determined earlier
                if writeErr := writeLastIP(cacheFilePath, currentIP); writeErr != nil {
                        // Log cache write failure but don't fail the whole process
                        log.Printf("[%s] ⚠️ Warning: Cloudflare update succeeded, but failed to write current IP to cache file '%s':: %v", time.Now().Format("2006-01-02 15:04:05"), cacheFilePath, writeErr)
                }
                log.Printf("[%s] ========= Cloudflare DDNS Update Completed Successfully =========", time.Now().Format("2006-01-02 15:04:05"))
        } else {
                log.Printf("[%s] ❌ DDNS update process failed. Check previous error messages.", time.Now().Format("2006-01-02 15:04:05"))
                log.Printf("[%s] ========= Cloudflare DDNS Update Failed =========", time.Now().Format("2006-01-02 15:04:05"))
                os.Exit(1) // Exit with error status if Cloudflare update failed
        }
}
