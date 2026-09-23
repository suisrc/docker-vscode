#!/bin/bash

## 这是 ms 官方版本，并且在使用中自动加载最新版本
## vscode - kvs 反向代理 (端口 KVS_VSCODE_PORT:7080, 目录 ${KVS_VSCODE_HOME:-~/.vscode-server}, PASSWORD:无需密码)

# source /etc/profile

# PASSWORD is empty, randomly generate a password
if [[ -z "${PASSWORD}" ]]; then
    export PASSWORD=$(openssl rand -hex 16)
    echo "PASSWORD is empty, randomly generate a password: ${PASSWORD}"
fi

if [[ -z "${KVS_VSCODE_HOME}" ]]; then
    # export KVS_VSCODE_HOME=`echo ~/.vscode-server`
    export KVS_VSCODE_HOME="/wsc/.vsc"
    echo "KVS_VSCODE_HOME is empty, default is ${KVS_VSCODE_HOME}"
fi

# 判断 "~/.vscode-server/data/Machine/settings.json" 是否存在，如果不存在则创建一个默认的 settings.json 文件
if [ ! -f "${KVS_VSCODE_HOME}/data/Machine/settings.json" ]; then
    mkdir -p "${KVS_VSCODE_HOME}/data/Machine"
    # 使用带引号的定界符 <<'EOF'，防止 heredoc 内的 ${input:...} 被 shell 当作变量展开
    cat <<'EOF' > "${KVS_VSCODE_HOME}/data/Machine/settings.json"
{
  "chat.allowAnonymousAccess": true,
  "chat.agentHost.allowSignedOutWhenUsable": true,
  "chat.agentHost.byokModels.enabled": true,
  "chat.agentHost.codexAgent.enabled": true,
  "chat.agentHost.claudeAgent.enabled": true,
  "chat.editor.codex.preferAgentHost": true,
  "chat.byokUtilityModelDefault": "mainAgent",
  "chat.disableAIFeatures": false,
  "terminal.integrated.scrollback": 10000,
  "terminal.integrated.defaultProfile.linux": "zsh",
  "git.ignoreLegacyWarning": true,
  "git.enableSmartCommit": true,
  "files.autoSave": "off",
  "editor.renderWhitespace": "all",
  "editor.suggestSelection": "first",
  "editor.fontSize": 16,
  "editor.fontLigatures": false,
  "explorer.confirmDelete": true,
  "extensions.autoUpdate": "off",
  "extensions.autoCheckUpdates": false,
  "workbench.colorTheme": "Dark+",
  "workbench.experimental.modernUI": false,
  "github.copilot.enable": { "*": false },
  "kaicustomendpoint.inlineCompletion": {
    "prompt": "You are a coding assistant, good at completing concise and efficient {languageId} code.\nprefix: {prefix}\nsuffix: {suffix}",
    "model": {
      "apiKey": "${input:chat.lm.secret.deepleek}",
      "id": "deepseek-flash",
      "name": "fim-deepseek",
      "url": "https://api.deepseek.com//beta/completions",
      "defaultReasoningEffort": ""
    }
  },
  "kaicustomendpoint.models": {
    "alibaba": "sk-xxx",
    "deepseek": "sk-xxx",
    "bigmodel": "xx.xxx"
  },
  "kaicustomendpoint.models": [
    {
      "name": "alibaba",
      "vendor": "customendpoint",
      "apiKey": "${input:chat.lm.secret.alibaba}",
      "apiType": "messages",
      "models": [
        {
          "id": "qwen3.8-flash",
          "name": "kai-qwen3.8-flash",
          "url": "https://dashscope.aliyuncs.com/apps/anthropic",
          "vision": true,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "defaultReasoningEffort": "high",
          "supportsReasoningEffort": ["none", "low", "medium", "high", "xhigh", "max"]
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
          "id": "deepseek-flash",
          "name": "kai-deepseek-flash",
          "url": "https://api.deepseek.com/anthropic",
          "vision": true,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "defaultReasoningEffort": "high",
          "supportsReasoningEffort": ["none", "low", "high", "max"]
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
          "id": "glm-5.3-flash",
          "name": "kai-glm-5.3-flash",
          "url": "https://open.bigmodel.cn/api/anthropic",
          "vision": true,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "defaultReasoningEffort": "high",
          "supportsReasoningEffort": ["none", "low", "medium", "high", "xhigh", "max"]
        }
      ]
    },
    {
      "name": "deepseek-openai",
      "vendor": "customendpoint",
      "apiKey": "${input:chat.lm.secret.deepseek}",
      "apiType": "chat-completions",
      "models": [
        {
          "id": "deepseek-flash",
          "name": "kai-deepseek-openai",
          "url": "https://api.deepseek.com",
          "vision": true,
          "maxInputTokens": 1000000,
          "maxOutputTokens": 100000,
          "defaultReasoningEffort": "high",
          "supportsReasoningEffort": ["none", "low", "high", "max", "high-op"],
          "metadata": {"user_id": "user-0001"}
        }
      ]
    }
  ]
}

EOF
fi

# kvs 是一个用于授权的工具，它会在启动 vscode server 前进行授权验证，确保只有通过验证的用户才能访问 vscode server
echo 'start vscode server. wss need set env: KVS_SVC_HEADER_X_FORWARDED_PORT=443'
KVS_HOME="${KVS_VSCODE_HOME}" KVS_LOGIN_AUTHZ=true KVS_SVC_SOCK_FILE="${KVS_VSCODE_HOME}/kvs.sock" \
KVS_PORT="${KVS_VSCODE_PORT:-7080}" KVS_COOKIE=vscode-tkn KVS_LOGIN_TOKEN="${PASSWORD}" \
exec kvs -c vscode
