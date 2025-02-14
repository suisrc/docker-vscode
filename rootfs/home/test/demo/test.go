/**
 * go mod init test
 * go run test.go
 * go build
 */
package main

import "fmt"

func main() {
	fmt.Println("Hello, 世界")
}

/**
  * gin
  * https://gin-gonic.com/zh-cn/docs/quickstart/
  * https://github.com/gin-gonic/gin
  package main

  import "github.com/gin-gonic/gin"

  func main() {
	  // gin.SetMode(gin.ReleaseMode)
	  r := gin.Default()
	  r.GET("/hello", func(c *gin.Context) {
		  c.JSON(200, gin.H{
			  "message": "hello gin",
		  })
	  })
	  r.Run() // 监听并在 0.0.0.0:8080 上启动服务
  }
*/

/**
   * iris
   * https://iris-go.com/
   * https://studyiris.com/
   * https://github.com/kataras/iris
  package main

  import (
	  "github.com/kataras/iris/v12"

	  "github.com/kataras/iris/v12/middleware/logger"
	  "github.com/kataras/iris/v12/middleware/recover"
  )

  func main() {
	  app := iris.New()
	  app.Logger().SetLevel("debug")
	  // Optionally, add two built'n handlers
	  // that can recover from any http-relative panics
	  // and log the requests to the terminal.
	  app.Use(recover.New())
	  app.Use(logger.New())

	  // Method:   GET
	  // Resource: http://localhost:8080
	  app.Handle("GET", "/", func(ctx iris.Context) {
		  ctx.HTML("<h1>Welcome</h1>")
	  })

	  // same as app.Handle("GET", "/ping", [...])
	  // Method:   GET
	  // Resource: http://localhost:8080/ping
	  app.Get("/ping", func(ctx iris.Context) {
		  ctx.WriteString("pong")
	  })

	  // Method:   GET
	  // Resource: http://localhost:8080/hello
	  app.Get("/hello", func(ctx iris.Context) {
		  ctx.JSON(iris.Map{"message": "Hello Iris!"})
	  })

	  // http://localhost:8080
	  // http://localhost:8080/ping
	  // http://localhost:8080/hello
	  app.Run(iris.Addr(":8080"), iris.WithoutServerError(iris.ErrServerClosed))
  }
*/
