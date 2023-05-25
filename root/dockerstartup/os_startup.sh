#!/usr/bin/env bash
set -ex

# 自定义配置，运行在所有脚本之前
function custom_config (){
    custom_config_script=/dockerstartup/custom_config.sh
    if [ -f "$custom_config_script" ]; then
        if [ ! -x "$custom_config_script" ]; then
            # echo "${custom_config}: not executable, exiting"
            # exit 1
            chmod +x "$custom_config_script"
        fi
        echo "Executing custom config: '$custom_config_script'"
        "$custom_config_script"
    fi
}

custom_config

exec "$@"
