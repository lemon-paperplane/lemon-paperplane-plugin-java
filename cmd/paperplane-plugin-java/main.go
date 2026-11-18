// paperplane-plugin-java 是 java 插件的独立可执行入口。
package main

import (
	sdk "github.com/lemon-paperplane/lemon-paperplane-main/lemon-paperplane-main-plugin-sdk"

	java "github.com/lemon-paperplane/lemon-paperplane-plugin-java/plugin"
)

func main() { sdk.Serve(java.New()) }
