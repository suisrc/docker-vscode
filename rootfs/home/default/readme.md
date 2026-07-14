# 说明 WB

## vscode 字体乱码

```sh settings.json 中增加
# vscode settings.json /root/.config/Code/User/settings.json
{
    "chat.allowAnonymousAccess": true,
    "terminal.integrated.scrollback": 10000,
    "terminal.integrated.defaultProfile.linux": "zsh",
    "terminal.integrated.fontFamily": "'Noto Sans Mono', 'PowerlineSymbols'",
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
    "workbench.colorTheme": "Dark+"
}
```