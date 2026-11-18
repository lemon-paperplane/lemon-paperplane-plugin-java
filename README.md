# lemon-paperplane-plugin-java

Lemon Paperplane 的 Java 插件。

- 工具链 `jdk`：发现本机 JDK（`/usr/libexec/java_home -X`、`~/Library/Java/JavaVirtualMachines`、`/Library/Java/JavaVirtualMachines`、SDKMAN、`~/.gradle/jdks`、`~/.jdks`、Homebrew、`/usr/lib/jvm`、Windows 的 Program Files、`JAVA_HOME`），读 `release` 文件得版本 / 厂商 / 架构，符号链接去重；下载 Temurin（Adoptium API 取文件名与 sha256，镜像在前）。
- facet `java`、运行配置 `java-application`（扫描 `public static void main`，`@SpringBootApplication` 交给 spring-boot 插件）、导入 IDEA 的 Application 运行配置。
- 运行：Gradle/Maven 工程经 `gradle` / `maven` 插件的 `prepareClasspath` 服务编译并取类路径；无构建工具的目录用 javac 编译到插件缓存目录（不写工程）。
- 服务 `buildCommand`：`{jdk, classpath, mainClass, vmOptions, programArgs, extraVmArgs, extraArgs, cwd, key}` → `LaunchSpec`。JDK 9+ 用 `@argfile`，JDK 8 用只含 `Class-Path` 清单的 jar，避免命令行超长；默认补 `-Dfile.encoding=UTF-8`。
- 诊断：类文件版本过新、无效的 release、老源码版本、Lombok 与 JDK 不兼容、堆/元空间不足（custom 修复在原有 VM 参数上加大）、找不到主类、端口占用（含中文 javac/java 报错）。

```bash
./build.sh
pp-probe -plugins . toolchains jdk
```
