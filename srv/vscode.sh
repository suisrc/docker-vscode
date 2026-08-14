#!/bin/bash

## 这是 ms 官方版本，并且在使用中自动加载最新版本

# source /etc/profile

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

# 判断 "${VSCODE_WSC:-/wsc}/.vsc/data/Machine/settings.json" 是否存在，如果不存在则创建一个默认的 settings.json 文件
if [ ! -f "${VSCODE_WSC:-/wsc}/.vsc/data/Machine/settings.json" ]; then
    mkdir -p "${VSCODE_WSC:-/wsc}/.vsc/data/Machine"
    # 使用带引号的定界符 <<'EOF'，防止 heredoc 内的 ${input:...} 被 shell 当作变量展开
    cat <<'EOF' > "${VSCODE_WSC:-/wsc}/.vsc/data/Machine/settings.json"
{
  "chat.allowAnonymousAccess": true,
  "terminal.integrated.scrollback": 10000,
  "terminal.integrated.defaultProfile.linux": "zsh",
  "git.ignoreLegacyWarning": true,
  "git.enableSmartCommit": true,
  "files.autoSave": "off",
  "editor.renderWhitespace": "all",
  "editor.suggestSelection": "first",
  "editor.fontSize": 16,
  "editor.fontLigatures": false,
  "explorer.confirmDelete": false,
  "extensions.autoUpdate": "off",
  "extensions.autoCheckUpdates": false,
  "workbench.colorTheme": "Dark+",
  "workbench.experimental.modernUI": false,
  "github.copilot.enable": { "*": false },
  "kaicustomendpoint.inlineCompletion": {
    "model": {
      "apiKey": "${input:chat.lm.secret.deepleek}",
      "id": "deepseek-v4-flash",
      "name": "fim-deepseek-v4",
      "url": "https://api.deepseek.com//beta/completions",
      "defaultReasoningEffort": ""
    }
  },
  "kaicustomendpoint.models": [
    {
      "name": "alibaba",
      "vendor": "customendpoint",
      "apiKey": "${input:chat.lm.secret.alibaba}",
      "apiType": "messages",
      "models": [
        {
          "id": "qwen3.7-plus",
          "name": "kai-qwen3.7-plus",
          "url": "https://dashscope.aliyuncs.com/apps/anthropic",
          "toolCalling": true,
          "vision": true,
          "maxInputTokens": 256000,
          "maxOutputTokens": 16000,
          "supportsReasoningEffort": ["low", "medium", "high", "xhigh"]
        }
      ]
    },
    {
      "name": "deepseek",
      "vendor": "customendpoint",
      "apiKey": "${input:chat.lm.secret.deepseek}",
      "apiType": "messages",
      "models": [
        {
          "id": "deepseek-v4-flash",
          "name": "kai-deepseek-v4-flash",
          "url": "https://api.deepseek.com/anthropic",
          "toolCalling": true,
          "vision": false,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "supportsReasoningEffort": ["low", "medium", "high", "xhigh"]
        },
        {
          "id": "deepseek-v4-pro",
          "name": "kai-deepseek-v4-pro",
          "url": "https://api.deepseek.com/anthropic",
          "toolCalling": true,
          "vision": false,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "supportsReasoningEffort": ["low", "medium", "high", "xhigh"]
        }
      ]
    },
    {
      "name": "bigmodel",
      "vendor": "customendpoint",
      "apiKey": "${input:chat.lm.secret.bigmodel}",
      "apiType": "messages",
      "models": [
        {
          "id": "glm-5.3",
          "name": "kai-glm-5.3",
          "url": "https://open.bigmodel.cn/api/anthropic",
          "toolCalling": true,
          "vision": false,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "supportsReasoningEffort": ["low", "medium", "high", "xhigh"]
        },
        {
          "id": "glm-5.2",
          "name": "kai-glm-5.2",
          "url": "https://open.bigmodel.cn/api/anthropic",
          "toolCalling": true,
          "vision": false,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "supportsReasoningEffort": ["low", "medium", "high", "xhigh"]
        },
        {
          "id": "glm-5v-turbo",
          "name": "kai-glm-5v-turbo",
          "url": "https://open.bigmodel.cn/api/anthropic",
          "toolCalling": true,
          "vision": true,
          "maxInputTokens": 200000,
          "maxOutputTokens": 32000,
          "supportsReasoningEffort": ["low", "medium", "high", "xhigh"]
        }
      ]
    }
  ]
}

EOF
fi

# kvs 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start vscode server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_SVC_SOCK_FILE=/var/run/vscode.sock KVS_HOME="${VSCODE_WSC:-/wsc}/.vsc" KVS_LOGIN_AUTHZ=true \
KVS_PORT="${VSCODE_PORT:-7080}" KVS_COOKIE=vscode-tkn KVS_LOGIN_TOKEN="${PASSWORD}" \
exec kvs -c default
