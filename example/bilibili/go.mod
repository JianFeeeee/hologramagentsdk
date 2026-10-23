module bilibili-plugin

go 1.25.0

require github.com/JianFeeeee/hologramagentsdk v0.0.0

// 与同目录其它示例一致：SDK 指向仓库内的 vendored 副本，
// 保证 `hmapdev build` 在干净环境（无 go.sum、无 proxy）下也能编。
replace github.com/JianFeeeee/hologramagentsdk => ../../
