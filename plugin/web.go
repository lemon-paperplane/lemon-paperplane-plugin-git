package git

import (
	"net/url"
	"strings"
)

// webBaseURL 把远程地址转成仓库网页地址：
// git@github.com:owner/repo.git、ssh://git@host:22/owner/repo.git、https://user@host/owner/repo.git → https://host/owner/repo。
// 认不出（本地路径等）返回空。
func webBaseURL(remote string) string {
	s := strings.TrimSpace(remote)
	if s == "" {
		return ""
	}
	var host, path string
	switch {
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") || strings.HasPrefix(s, "ssh://") || strings.HasPrefix(s, "git://"):
		u, err := url.Parse(s)
		if err != nil || u.Host == "" {
			return ""
		}
		host = u.Hostname()
		if strings.HasPrefix(s, "http") && u.Port() != "" {
			host = u.Host
		}
		path = u.Path
	case strings.Contains(s, ":") && !strings.Contains(strings.SplitN(s, ":", 2)[0], "/") && !isWindowsDrive(s):
		// scp 风格：[user@]host:path
		hp := strings.SplitN(s, ":", 2)
		host = hp[0]
		if i := strings.LastIndex(host, "@"); i >= 0 {
			host = host[i+1:]
		}
		path = "/" + strings.TrimPrefix(hp[1], "/")
	default:
		return ""
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if host == "" || path == "" || path == "/" {
		return ""
	}
	// 常见托管的 ssh 主机名别名
	switch host {
	case "ssh.github.com":
		host = "github.com"
	case "altssh.gitlab.com":
		host = "gitlab.com"
	}
	// Azure DevOps：ssh.dev.azure.com:v3/org/project/repo → dev.azure.com/org/project/_git/repo
	if host == "ssh.dev.azure.com" {
		parts := strings.Split(strings.TrimPrefix(path, "/v3/"), "/")
		if len(parts) == 3 {
			return "https://dev.azure.com/" + parts[0] + "/" + parts[1] + "/_git/" + parts[2]
		}
	}
	return "https://" + host + path
}

func isWindowsDrive(s string) bool {
	return len(s) >= 2 && s[1] == ':' && (s[0] >= 'A' && s[0] <= 'Z' || s[0] >= 'a' && s[0] <= 'z')
}

// hostKind 按主机名猜托管平台（决定提交/文件页面的路径格式）。
func hostKind(base string) string {
	h := strings.ToLower(base)
	switch {
	case strings.Contains(h, "gitlab"):
		return "gitlab"
	case strings.Contains(h, "bitbucket"):
		return "bitbucket"
	case strings.Contains(h, "dev.azure.com") || strings.Contains(h, "visualstudio.com"):
		return "azure"
	case strings.Contains(h, "codeup.aliyun.com"):
		return "codeup"
	default:
		// GitHub、Gitee、Gitea、Codeberg、GitCode 等都是 /commit/<hash>、/blob/<ref>/<path>
		return "github"
	}
}

func webCommitURL(base, hash string) string {
	if base == "" {
		return ""
	}
	switch hostKind(base) {
	case "gitlab", "codeup":
		return base + "/-/commit/" + hash
	case "bitbucket":
		return base + "/commits/" + hash
	default:
		return base + "/commit/" + hash
	}
}

func webFileURL(base, ref, path string) string {
	if base == "" {
		return ""
	}
	p := escapePath(path)
	switch hostKind(base) {
	case "gitlab", "codeup":
		return base + "/-/blob/" + ref + "/" + p
	case "bitbucket":
		return base + "/src/" + ref + "/" + p
	case "azure":
		return base + "?path=/" + url.QueryEscape(path) + "&version=GC" + ref
	default:
		return base + "/blob/" + ref + "/" + p
	}
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
