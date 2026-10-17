# editdoc · 全能办公

对 `.docx` / `.xlsx` / `.pptx` / `.xls` / `.csv` / `.md` / `.txt` 做**新建、读取、编辑、转换**。

> **版本**：v2.0.0（全能办公）。较 v1.0.0 新增 `read_document`、`create_document`、
> `create_from_template`、`convert_document`、`office_check` 五个工具；
> `edit_document` 的名字与全部 v1 参数**保持不变**，老调用不会破。

## 工具

| 工具 | 作用 |
|---|---|
| `read_document` | 读为结构化 JSON（docx 段落+表格+页眉页脚 / xlsx 逐表逐格含公式与计算值 / pptx 逐页 / xls / csv / md / txt），大文件可 `offset`/`limit` 分段 |
| `create_document` | 从零新建。docx 支持 Markdown 正文；xlsx 支持多表（表名+表头+行）；pptx 用 `##` 分页 |
| `create_from_template` | 复制模板再套用替换 |
| `edit_document` | 编辑既有文件（v1 八参数 + 查找替换/正则大小写/第 N 处/表格与单元格/图片/样式/幻灯片/`dry_run`） |
| `convert_document` | 格式转换（docx↔md/pdf、xlsx↔csv 等，借助 soffice / pandoc） |
| `office_check` | 自检：依赖库可用性与版本、外部工具、格式×能力矩阵 |

## 配置项

| 键 | 默认 | 说明 |
|---|---|---|
| `script_path` | 空 | `edit_doc.py` 的绝对路径。**留空即推荐值** —— 用内嵌脚本；仅当该文件含 v2 协议标记时才被采用，否则自动忽略并回退内嵌 |
| `venv_python` | 空 | 执行脚本的 Python 解释器（需装 python-docx / openpyxl / python-pptx）。**必须配置** |
| `timeout_sec` | 60 | 单次调用超时；大文件或格式转换（soffice 首次启动慢）建议调到 180 |

## 工作原理

Go 侧只做参数编排与结构化返回，全部业务逻辑在 Python 脚本 `edit_doc.py` 里。

**脚本是内嵌的**（`//go:embed embed/edit_doc.py`），原因：`hmapdev` 打包器只把
`plugin.json` + `plugin.bin` 放进 `.hmap`，**不打包任何资源文件**。若让脚本单独
存在于 `plugin_data/`，就会出现「Go 侧升级了、脚本忘了换」的版本漂移。
内嵌 + 内容指纹校验使两侧永远同版本：

1. 首次调用时把内嵌脚本释放到 `DataDir()/runtime/edit_doc.py`，并写 `.stamp` 指纹；
2. 插件升级后指纹变化 → 自动重新释放；
3. `script_path` 显式指向的文件**仅当含 `EDITDOC_PROTOCOL = "v2"` 标记时**才被采用
   （便于运维临时覆盖调试）；否则忽略并在返回体里带 `script_path_ignored`，让降级可见。

**业务脚本只有一份**：`embed/edit_doc.py`。不要再添加同名的顶层副本 ——
它不会被 embed，改了不生效，而两份同名文件是典型的漂移源（见
`TestOnlyOneBusinessScript`）。

## 依赖

Python 侧：`python-docx`、`openpyxl`、`python-pptx`（`.xls` 另需 `xlrd`）。
外部工具（可选，转换能力用）：`soffice`（LibreOffice）、`pandoc`。
用 `office_check` 随时查看本机实际可用的能力矩阵。

## 构建

```bash
hmapdev build
```

## 测试

```bash
go test ./...
```
