#!/bin/bash

# 等待桌面环境启动完成
# /usr/bin/desktop_ready

# 如果没有nginx进程，启动nginx
# /usr/local/openresty/nginx/sbin/nginx -> /usr/local/bin/nginx
if [ ! pgrep nginx ]; then
    echo "nginx is not running, start it..."
    nginx -g "daemon off;" &
else
    echo "nginx is running, no need to start"
fi

# 监控nginx配置文件变化，自动reload
inotifywait -e modify,move,create,delete -mr --timefmt '%d/%m/%y %H:%M' --format '%T' /etc/nginx/conf.d/ | while read date time; do
    echo "At ${time} on ${date}, config file update detected."
    nginx -s reload
done

##  # dict to store processes
##  declare -A CUSTOM_PROCS
##  
##  function custom_startup (){
##      custom_startup_script=/dockerstartup/custom_startup.sh
##      if [ -f "$custom_startup_script" ]; then
##          if [ ! -x "$custom_startup_script" ]; then
##              echo "${custom_startup_script}: not executable, exiting"
##              exit 1
##          fi
##  
##          "$custom_startup_script" &
##          CUSTOM_PROCS['custom_startup']=$!
##      fi
##  }
##  
##  # Start processes
##  custom_startup
##  
##  
##  # Monitor Kasm Services
##  sleep 3
##  while :
##  do
##      for process in "${!CUSTOM_PROCS[@]}"; do
##          if ! kill -0 "${CUSTOM_PROCS[$process]}" ; then
##  
##              # If DLP Policy is set to fail secure, default is to be resilient
##              if [[ ${DLP_PROCESS_FAIL_SECURE:-0} == 1 ]]; then
##                  exit 1
##              fi
##  
##              case $process in
##                  custom_script)
##                      echo "The custom startup script exited."
##                      # custom startup scripts track the target process on their own, they should not exit
##                      custom_startup
##                      ;;
##                  *)
##                      echo "Unknown Service: $process"
##                      ;;
##              esac
##          fi
##      done
##      sleep 3
##  done
##  
##  
##  echo "Exiting container"

