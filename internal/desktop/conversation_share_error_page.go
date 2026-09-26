package desktop

import (
	"html"
	"net/http"
)

type publicConversationShareErrorCopy struct {
	title       string
	description string
	hint        string
}

var (
	publicConversationShareNotFoundDocument = buildPublicConversationShareErrorDocument(
		publicConversationShareErrorCopy{
			title:       "分享不可用",
			description: "该分享可能已过期、被撤销，或链接不正确。",
			hint:        "请向分享者确认链接是否仍然有效。",
		},
	)
	publicConversationShareInternalErrorDocument = buildPublicConversationShareErrorDocument(
		publicConversationShareErrorCopy{
			title:       "暂时无法打开分享",
			description: "服务暂时不可用，请稍后再试。",
			hint:        "如果问题持续存在，请联系分享者。",
		},
	)
	publicConversationShareMethodNotAllowedDocument = buildPublicConversationShareErrorDocument(
		publicConversationShareErrorCopy{
			title:       "无法打开此页面",
			description: "当前请求方式不受支持。",
			hint:        "请直接在浏览器中打开完整的分享链接。",
		},
	)
)

func publicConversationShareErrorDocument(status int) []byte {
	switch status {
	case http.StatusNotFound:
		return publicConversationShareNotFoundDocument
	case http.StatusMethodNotAllowed:
		return publicConversationShareMethodNotAllowedDocument
	default:
		return publicConversationShareInternalErrorDocument
	}
}

func buildPublicConversationShareErrorDocument(copy publicConversationShareErrorCopy) []byte {
	title := html.EscapeString(copy.title)
	description := html.EscapeString(copy.description)
	hint := html.EscapeString(copy.hint)
	return []byte(`<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
  <meta name="color-scheme" content="light dark">
  <meta name="robots" content="noindex,nofollow,noarchive">
  <meta name="referrer" content="no-referrer">
  <title>` + title + `</title>
  <style>
    :root{color-scheme:light dark;--page:#f5f7fb;--surface:#fff;--text:#171a21;--muted:#667085;--line:#e4e8ef;--accent:#3559e0;--accent-soft:#eef2ff;--shadow:0 20px 60px rgba(29,39,68,.12)}
    @media(prefers-color-scheme:dark){:root{--page:#111318;--surface:#1b1f27;--text:#f1f4f8;--muted:#a2aaba;--line:#303642;--accent:#86a4ff;--accent-soft:#252d45;--shadow:0 24px 72px rgba(0,0,0,.34)}}
    *{box-sizing:border-box}
    html,body{min-width:280px;min-height:100%;margin:0}
    body{background:var(--page);color:var(--text);font-family:Inter,ui-sans-serif,-apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif}
    .share-error-page{display:grid;min-height:100vh;place-items:center;padding:calc(28px + env(safe-area-inset-top)) 20px calc(28px + env(safe-area-inset-bottom))}
    .share-error-shell{width:min(100%,520px)}
    .share-error-context{margin:0 0 18px;color:var(--muted);font-size:13px;font-weight:650;letter-spacing:.02em;text-align:center}
    .share-error-card{padding:46px 38px 40px;border:1px solid var(--line);border-radius:22px;background:var(--surface);box-shadow:var(--shadow);text-align:center}
    .share-error-icon{display:grid;width:64px;height:64px;margin:0 auto 24px;place-items:center;border-radius:20px;background:var(--accent-soft);color:var(--accent)}
    .share-error-icon svg{width:30px;height:30px;fill:none;stroke:currentColor;stroke-linecap:round;stroke-linejoin:round;stroke-width:1.8}
    h1{margin:0;color:var(--text);font-size:24px;line-height:1.35;letter-spacing:-.02em}
    .share-error-description{margin:14px auto 0;color:var(--muted);font-size:15px;line-height:1.75}
    .share-error-hint{margin:26px 0 0;padding-top:22px;border-top:1px solid var(--line);color:var(--muted);font-size:13px;line-height:1.65}
    @media(max-width:520px){.share-error-card{padding:38px 24px 32px;border-radius:18px}.share-error-icon{width:58px;height:58px;border-radius:18px}h1{font-size:22px}.share-error-description{font-size:14px}}
  </style>
</head>
<body>
  <main class="share-error-page">
    <div class="share-error-shell">
      <p class="share-error-context">Shared conversation</p>
      <section class="share-error-card" aria-labelledby="share-error-title">
        <div class="share-error-icon" aria-hidden="true">
          <svg viewBox="0 0 24 24"><path d="M9.5 14.5l5-5"/><path d="M7.2 17.8l-1 1a3.5 3.5 0 01-5-5l3.1-3.1a3.5 3.5 0 014.9 0"/><path d="M16.8 6.2l1-1a3.5 3.5 0 115 5l-3.1 3.1a3.5 3.5 0 01-4.9 0"/></svg>
        </div>
        <h1 id="share-error-title">` + title + `</h1>
        <p class="share-error-description">` + description + `</p>
        <p class="share-error-hint">` + hint + `</p>
      </section>
    </div>
  </main>
</body>
</html>`)
}
