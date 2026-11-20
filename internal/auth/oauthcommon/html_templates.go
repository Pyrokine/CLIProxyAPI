package oauthcommon

import (
	"html"
	"strings"
)

// loginSuccessHTML is the HTML template displayed to users after successful OAuth authentication.
// The placeholder {{PROVIDER_NAME}} is replaced with the actual provider name (e.g. "Claude", "Codex").
//
//goland:noinspection SpellCheckingInspection
const loginSuccessHTML = "<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n    <meta charset=\"UTF-8\">\n    <" +
	"meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\">\n    <title>Authentication " +
	"Successful - {{PROVIDER_NAME}}</title>\n    <link rel=\"icon\" type=\"image/svg+xml\" href=\"data:image/" +
	"svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 24 24' fill='%2310b981'%3E%3Cpath d='M9 " +
	"12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z'/%3E%3C/svg%3E\">\n    <style>\n        * {\n            box-" +
	"sizing: border-box;\n        }\n        body {\n            font-family: -apple-system, " +
	"BlinkMacSystemFont, 'Segoe UI', Roboto, Oxygen, Ubuntu, Cantarell, sans-serif;\n            display: " +
	"flex;\n            justify-content: center;\n            align-items: center;\n            min-height: " +
	"100vh;\n            margin: 0;\n            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%" +
	");\n            padding: 1rem;\n        }\n        .container {\n            text-align: center;\n      " +
	"      background: white;\n            padding: 2.5rem;\n            border-radius: 12px;\n            " +
	"box-shadow: 0 10px 25px rgba(0,0,0,0.1);\n            max-width: 480px;\n            width: 100%;\n     " +
	"       animation: slideIn 0.3s ease-out;\n        }\n        @keyframes slideIn {\n            from {\n " +
	"               opacity: 0;\n                transform: translateY(-20px);\n            }\n            " +
	"to {\n                opacity: 1;\n                transform: translateY(0);\n            }\n        }\n" +
	"        .success-icon {\n            width: 64px;\n            height: 64px;\n            margin: 0 " +
	"auto 1.5rem;\n            background: #10b981;\n            border-radius: 50%;\n            display: " +
	"flex;\n            align-items: center;\n            justify-content: center;\n            color: white;" +
	"\n            font-size: 2rem;\n            font-weight: bold;\n        }\n        h1 {\n            " +
	"color: #1f2937;\n            margin-bottom: 1rem;\n            font-size: 1.75rem;\n            font-" +
	"weight: 600;\n        }\n        .subtitle {\n            color: #6b7280;\n            margin-bottom: 1." +
	"5rem;\n            font-size: 1rem;\n            line-height: 1.5;\n        }\n        .setup-notice {\n" +
	"            background: #fef3c7;\n            border: 1px solid #f59e0b;\n            border-radius: " +
	"6px;\n            padding: 1rem;\n            margin: 1rem 0;\n        }\n        .setup-notice h3 {\n  " +
	"          color: #92400e;\n            margin: 0 0 0.5rem 0;\n            font-size: 1rem;\n        }\n " +
	"       .setup-notice p {\n            color: #92400e;\n            margin: 0;\n            font-size: 0." +
	"875rem;\n        }\n        .setup-notice a {\n            color: #1d4ed8;\n            text-decoration:" +
	" none;\n        }\n        .setup-notice a:hover {\n            text-decoration: underline;\n        }\n" +
	"        .actions {\n            display: flex;\n            gap: 1rem;\n            justify-content: " +
	"center;\n            flex-wrap: wrap;\n            margin-top: 2rem;\n        }\n        .button {\n    " +
	"        padding: 0.75rem 1.5rem;\n            border-radius: 8px;\n            font-size: 0.875rem;\n   " +
	"         font-weight: 500;\n            text-decoration: none;\n            transition: all 0.2s;\n     " +
	"       cursor: pointer;\n            border: none;\n            display: inline-flex;\n            " +
	"align-items: center;\n            gap: 0.5rem;\n        }\n        .button-primary {\n            " +
	"background: #3b82f6;\n            color: white;\n        }\n        .button-primary:hover {\n           " +
	" background: #2563eb;\n            transform: translateY(-1px);\n        }\n        .button-secondary {" +
	"\n            background: #f3f4f6;\n            color: #374151;\n            border: 1px solid #d1d5db;" +
	"\n        }\n        .button-secondary:hover {\n            background: #e5e7eb;\n        }\n        ." +
	"countdown {\n            color: #9ca3af;\n            font-size: 0.75rem;\n            margin-top: 1rem;" +
	"\n        }\n        .footer {\n            margin-top: 2rem;\n            padding-top: 1.5rem;\n       " +
	"     border-top: 1px solid #e5e7eb;\n            color: #9ca3af;\n            font-size: 0.75rem;\n     " +
	"   }\n        .footer a {\n            color: #3b82f6;\n            text-decoration: none;\n        }\n " +
	"       .footer a:hover {\n            text-decoration: underline;\n        }\n    </style>\n</head>\n<" +
	"body>\n    <div class=\"container\">\n        <div class=\"success-icon\">✓</div>\n        <h1>" +
	"Authentication Successful!</h1>\n        <p class=\"subtitle\">You have successfully authenticated with " +
	"{{PROVIDER_NAME}}. You can now close this window and return to your terminal to continue.</p>\n\n       " +
	" {{SETUP_NOTICE}}\n\n        <div class=\"actions\">\n            <button class=\"button button-" +
	"primary\" onclick=\"window.close()\">\n                <span>Close Window</span>\n            </button>" +
	"\n            <a href=\"{{PLATFORM_URL}}\" target=\"_blank\" class=\"button button-secondary\">\n       " +
	"         <span>Open Platform</span>\n                <span>↗</span>\n            </a>\n        </div>\n" +
	"\n        <div class=\"countdown\">\n            This window will close automatically in <span id=\"" +
	"countdown\">10</span> seconds\n        </div>\n\n        <div class=\"footer\">\n            <p>Powered " +
	"by <a href=\"https://chatgpt.com\" target=\"_blank\">ChatGPT</a></p>\n        </div>\n    </div>\n\n    " +
	"<script>\n        let countdown = 10;\n        const countdownElement = document.getElementById('" +
	"countdown');\n\n        const timer = setInterval(() => {\n            countdown--;\n            " +
	"countdownElement.textContent = countdown;\n\n            if (countdown <= 0) {\n                " +
	"clearInterval(timer);\n                window.close();\n            }\n        }, 1000);\n\n        // " +
	"Close window when user presses Escape\n        document.addEventListener('keydown', (e) => {\n          " +
	"  if (e.key === 'Escape') {\n                window.close();\n            }\n        });\n\n        // " +
	"Focus the close button for keyboard accessibility\n        document.querySelector('.button-primary')." +
	"focus();\n    </script>\n</body>\n</html>"

// setupNoticeHTML is the HTML template for the setup notice section.
const setupNoticeHTML = "\n        <div class=\"setup-notice\">\n            <h3>Additional Set" +
	"up Required</h3>\n            <p>To complete your setup, please visit " +
	`the <a href="{{PLATFORM_URL}}" target="_blank">{{PROVIDER_NAME}}</a> t` +
	"o configure your account.</p>\n        </div>"

// generateSuccessHTML creates the HTML content for the success page.
func generateSuccessHTML(providerName string, setupRequired bool, platformURL string) string {
	safeProviderName := html.EscapeString(providerName)
	safePlatformURL := html.EscapeString(platformURL)

	result := strings.ReplaceAll(loginSuccessHTML, "{{PROVIDER_NAME}}", safeProviderName)
	result = strings.ReplaceAll(result, "{{PLATFORM_URL}}", safePlatformURL)

	if setupRequired {
		setupNotice := strings.ReplaceAll(setupNoticeHTML, "{{PROVIDER_NAME}}", safeProviderName)
		setupNotice = strings.ReplaceAll(setupNotice, "{{PLATFORM_URL}}", safePlatformURL)
		result = strings.Replace(result, "{{SETUP_NOTICE}}", setupNotice, 1)
	} else {
		result = strings.Replace(result, "{{SETUP_NOTICE}}", "", 1)
	}

	return result
}
