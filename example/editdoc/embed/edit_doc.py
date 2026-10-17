#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
editdoc v2.0.0 —— 全能办公插件后端脚本。

调用约定（Go 侧 plugin.go）：
    edit_doc.py <command> <args_json>
    command ∈ read | create | edit | convert | check

输出：stdout 上一行 JSON。约定统一为
    {"ok": bool, ...payload}
失败时：{"ok": false, "error": str, "hint": str}

设计要点：
  * 所有写操作沿用 v1 的「.bak 留档 + 临时文件原子替换」策略，不许退化。
  * 错误一律结构化（ok=false + error + hint），绝不把裸 traceback 扔给上层。
  * 依赖缺失时点名缺哪个包 + 给出 pip 命令，不静默失败。

命令与操作（args_json 里的 op 字段）：
  read    : 读任意支持格式为结构化 JSON（docx/xlsx/pptx/xls/csv/txt/md）
  create  : 从零新建（docx/xlsx/pptx/md/txt/csv）
  edit    : 编辑既有文件（见 OPS 表）
  convert : 格式转换（docx↔md、xlsx↔csv、pptx→文本，走 soffice/pandoc）
  check   : 依赖与能力自检（独立可调用）

协议标记（勿删）：
  Go 侧 plugin.go 的 resolveScript() 靠本标记判断「配置里 script_path 指向的脚本
  是不是 v2 脚本」。缺少它的文件（例如 v1 的 edit_doc.py，其 argv 约定为
  <file> <op> <json>）会被忽略并回退到内嵌脚本，避免把 6 个工具全部打挂。
"""

EDITDOC_PROTOCOL = "v2"

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from datetime import datetime

# ---------------------------------------------------------------- 基础工具

# 支持格式矩阵（供 check 与描述使用）
FORMATS = {
    "docx": ["read", "create", "edit", "convert"],
    "xlsx": ["read", "create", "edit", "convert"],
    "xlsm": ["read", "edit"],
    "pptx": ["read", "create", "edit", "convert"],
    "xls": ["read"],          # 老格式，仅读（需 xlrd）
    "csv": ["read", "create", "convert"],
    "md": ["read", "create", "convert"],
    "txt": ["read", "create"],
}

# 依赖探测表：(import 名, pip 名, 用途)
DEPS = [
    ("docx", "python-docx", "Word .docx 读写"),
    ("openpyxl", "openpyxl", "Excel .xlsx 读写"),
    ("pptx", "python-pptx", "PowerPoint .pptx 读写"),
    ("xlrd", "xlrd", "老式 .xls 只读"),
    ("markdown", "markdown", "Markdown → HTML（新建 docx 时的富文本）"),
]
# 外部命令行工具
BINS = [
    ("soffice", "LibreOffice（格式转换）"),
    ("pandoc", "Pandoc（Markdown 转换）"),
]


def emit(payload):
    """输出一行 JSON 并退出。"""
    sys.stdout.write(json.dumps(payload, ensure_ascii=False) + "\n")
    sys.stdout.flush()
    sys.exit(0)


def fail(error, hint="", **extra):
    d = {"ok": False, "error": str(error), "hint": hint}
    d.update(extra)
    emit(d)


def dep_ok(mod):
    try:
        __import__(mod)
        return True
    except Exception:
        return False


def require_dep(mod, pipname, purpose, fmt):
    """依赖检查：缺失时给出可执行的 pip 命令。"""
    if not dep_ok(mod):
        fail(
            "缺少 Python 依赖 %s（用于 %s）" % (pipname, purpose),
            "请执行: %s -m pip install %s" % (sys.executable, pipname),
            missing=[pipname], format=fmt,
        )


def which(binname):
    return shutil.which(binname)


def backup(path):
    """编辑前留档，避免改坏无法回滚。"""
    try:
        stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
        dst = "%s.bak-%s" % (path, stamp)
        shutil.copy2(path, dst)
        return dst
    except Exception:
        return None


def save_atomic(save_fn, path):
    """先写临时文件再替换，避免写一半损坏原文件。"""
    d = os.path.dirname(os.path.abspath(path)) or "."
    fd, tmp = tempfile.mkstemp(suffix=os.path.splitext(path)[1], dir=d)
    os.close(fd)
    try:
        save_fn(tmp)
        shutil.move(tmp, path)
    except Exception:
        if os.path.exists(tmp):
            try:
                os.remove(tmp)
            except Exception:
                pass
        raise


def ensure_dir(path):
    d = os.path.dirname(os.path.abspath(path))
    if d:
        os.makedirs(d, exist_ok=True)


def ext_of(path):
    return os.path.splitext(path)[1].lower().lstrip(".")


def match_text(hay, needle, case_sensitive=True, regex=False):
    """按参数匹配，返回命中次数。"""
    if not needle:
        return 0
    if regex:
        flags = 0 if case_sensitive else re.IGNORECASE
        return len(re.findall(needle, hay, flags))
    if case_sensitive:
        return hay.count(needle)
    return hay.lower().count(needle.lower())


def apply_text(hay, needle, repl, case_sensitive=True, regex=False, nth=None):
    """
    执行替换。nth 为 None 时全替换；为整数时只替换第 nth 个（1 基）。
    返回 (新文本, 实际替换处数)

    注意：返回的第二个值是「真正被替换掉的处数」，不是「匹配到多少处」。
    指定 nth=2 且文本里有 3 处匹配时，应为 1（只换了第 2 处）。
    （曾经返回匹配总数，导致上层误以为三处都被改了。）
    """
    if regex:
        flags = 0 if case_sensitive else re.IGNORECASE
        pat = re.compile(needle, flags)
        if nth is None:
            new, n = pat.subn(repl, hay)
            return new, n
        cnt = [0]

        def _r(m):
            cnt[0] += 1
            if cnt[0] == int(nth):
                cnt[0] = -cnt[0]  # 标记已替换，继续用绝对值计数
                return repl
            return m.group(0)

        new = pat.sub(_r, hay)
        return new, (1 if cnt[0] < 0 else 0)

    if nth is None:
        if case_sensitive:
            return hay.replace(needle, repl), hay.count(needle)
        pat = re.compile(re.escape(needle), re.IGNORECASE)
        return pat.sub(lambda m: repl, hay), len(pat.findall(hay))

    # 大小写敏感/不敏感 + nth：只换第 nth 个
    pat = re.compile(re.escape(needle), 0 if case_sensitive else re.IGNORECASE)
    seen = [0]
    hit = [False]

    def _r2(m):
        seen[0] += 1
        if seen[0] == int(nth):
            hit[0] = True
            return repl
        return m.group(0)

    new = pat.sub(_r2, hay)
    return new, (1 if hit[0] else 0)


# ---------------------------------------------------------------- read

def read_docx(path, offset=0, limit=0):
    from docx import Document

    doc = Document(path)
    blocks = []

    def para_dict(p, where):
        return {"type": "paragraph", "where": where, "style": p.style.name if p.style else None,
                "text": p.text}

    for p in doc.paragraphs:
        blocks.append(para_dict(p, "body"))

    for ti, t in enumerate(doc.tables):
        rows = []
        for row in t.rows:
            rows.append([c.text for c in row.cells])
        blocks.append({"type": "table", "index": ti, "rows": rows})

    for si, section in enumerate(doc.sections):
        for label, part in (("header", section.header), ("footer", section.footer)):
            try:
                for p in part.paragraphs:
                    if p.text.strip():
                        blocks.append(para_dict(p, "%s[%d]" % (label, si)))
            except Exception:
                pass

    total = len(blocks)
    if offset or limit:
        end = (offset + limit) if limit else total
        blocks = blocks[offset:end]

    return {"format": "docx", "total_blocks": total, "offset": offset,
            "returned": len(blocks), "blocks": blocks}


def read_xlsx(path, offset=0, limit=0, sheet=None, formulas=True):
    import openpyxl

    # data_only 同时取一次，用来看公式的计算结果
    wb = openpyxl.load_workbook(path, data_only=False)
    wbv = openpyxl.load_workbook(path, data_only=True)

    sheets_out = []
    names = [sheet] if (sheet and sheet in wb.sheetnames) else wb.sheetnames
    for name in names:
        ws = wb[name]
        wsv = wbv[name]
        rows = []
        for r_i, row in enumerate(ws.iter_rows(), start=1):
            cells = []
            for c in row:
                if c.value is None:
                    continue
                item = {"cell": c.coordinate, "value": c.value}
                if formulas and isinstance(c.value, str) and c.value.startswith("="):
                    # 附上计算值
                    try:
                        item["computed"] = wsv[c.coordinate].value
                    except Exception:
                        item["computed"] = None
                cells.append(item)
            if cells:
                rows.append({"row": r_i, "cells": cells})
        sheets_out.append({"name": name, "max_row": ws.max_row,
                           "max_col": ws.max_column, "rows": rows})

    if offset or limit:
        end = (offset + limit) if limit else None
        sheets_out = sheets_out[offset:end]

    return {"format": "xlsx", "sheetnames": wb.sheetnames, "sheets": sheets_out}


def read_pptx(path, offset=0, limit=0):
    from pptx import Presentation

    prs = Presentation(path)
    slides = []
    for i, slide in enumerate(prs.slides, start=1):
        texts, tables, images = [], [], 0
        for sh in slide.shapes:
            if getattr(sh, "has_text_frame", False) and sh.has_text_frame:
                t = sh.text_frame.text
                if t.strip():
                    texts.append(t)
            if getattr(sh, "has_table", False) and sh.has_table:
                rows = [[c.text for c in r.cells] for r in sh.table.rows]
                tables.append(rows)
            if sh.shape_type is not None and "PICTURE" in str(sh.shape_type):
                images += 1
        slides.append({"slide": i, "texts": texts, "tables": tables, "images": images})

    total = len(slides)
    if offset or limit:
        end = (offset + limit) if limit else total
        slides = slides[offset:end]

    return {"format": "pptx", "total_slides": total, "offset": offset,
            "returned": len(slides), "slides": slides}


def read_xls(path):
    require_dep("xlrd", "xlrd", "读取老式 .xls", "xls")
    import xlrd

    book = xlrd.open_workbook(path)
    sheets = []
    for sh in book.sheets():
        rows = []
        for r in range(sh.nrows):
            vals = [sh.cell_value(r, c) for c in range(sh.ncols)]
            if any(v != "" for v in vals):
                rows.append({"row": r + 1, "values": vals})
        sheets.append({"name": sh.name, "nrows": sh.nrows, "ncols": sh.ncols, "rows": rows})
    return {"format": "xls", "sheets": sheets}


def read_plain(path):
    with open(path, "r", encoding="utf-8", errors="replace") as f:
        content = f.read()
    lines = content.splitlines()
    return {"format": ext_of(path), "total_lines": len(lines), "content": content}


def op_read(a):
    path = a.get("file") or ""
    if not path:
        fail("read 需要 file 参数", "传入要读取的文件绝对路径")
    if not os.path.exists(path):
        fail("文件不存在: %s" % path, "先用 create 新建，或检查路径拼写",
             path=path, exists=False)
    e = ext_of(path)
    offset = int(a.get("offset") or 0)
    limit = int(a.get("limit") or 0)
    try:
        if e == "docx":
            r = read_docx(path, offset, limit)
        elif e in ("xlsx", "xlsm"):
            r = read_xlsx(path, offset, limit, a.get("sheet"),
                          formulas=bool(a.get("formulas", True)))
        elif e == "pptx":
            r = read_pptx(path, offset, limit)
        elif e == "xls":
            r = read_xls(path)
        elif e in ("csv", "txt", "md"):
            r = read_plain(path)
        else:
            fail("不支持读取的格式: .%s" % e,
                 "支持: " + ", ".join(sorted(FORMATS.keys())))
    except Exception as ex:
        fail("%s: %s" % (type(ex).__name__, ex),
             "文件可能损坏，或缺少对应 Python 库")
    r["ok"] = True
    r["file"] = path
    # 附一段人读摘要，方便 LLM 直接用
    r["summary"] = summarize_read(r)
    emit(r)


def summarize_read(r):
    f = r.get("format")
    if f == "docx":
        tops = [b["text"] for b in r["blocks"] if b["type"] == "paragraph" and b["text"].strip()][:5]
        return "docx 共 %d 个块，前几段: %s" % (r.get("total_blocks", 0), " | ".join(tops)[:300])
    if f == "xlsx":
        parts = ["表%s(%dx%d行)" % (s["name"], s["max_row"], s["max_col"]) for s in r["sheets"]]
        return "xlsx 工作表: " + ", ".join(parts)
    if f == "pptx":
        return "pptx 共 %d 页，已返回 %d 页" % (r.get("total_slides", 0), r.get("returned", 0))
    if f == "xls":
        return "xls 工作表: " + ", ".join(s["name"] for s in r["sheets"])
    return "%s 共 %d 行" % (f, r.get("total_lines", 0))


# ---------------------------------------------------------------- create

def md_to_plain_blocks(md):
    """把 Markdown 粗解析成 (kind, text) 列表：# → 标题，- → 列表，其余 → 段落。"""
    out = []
    for raw in md.splitlines():
        line = raw.rstrip()
        if not line.strip():
            continue
        m = re.match(r"^(#{1,6})\s+(.*)$", line)
        if m:
            out.append(("h%d" % len(m.group(1)), m.group(2).strip()))
            continue
        m = re.match(r"^\s*[-*+]\s+(.*)$", line)
        if m:
            out.append(("li", m.group(1).strip()))
            continue
        m = re.match(r"^\s*\d+[.)]\s+(.*)$", line)
        if m:
            out.append(("li", m.group(1).strip()))
            continue
        out.append(("p", line.strip()))
    return out


def md_inline_runs(text):
    """
    把 **粗体** / *斜体* / `代码` 解析成 [(text, bold, italic)] 段落 runs，供 docx 用。
    """
    runs = []
    pat = re.compile(r"(\*\*.+?\*\*|\*.+?\*|`.+?`)")
    pos = 0
    for m in pat.finditer(text):
        if m.start() > pos:
            runs.append((text[pos:m.start()], False, False))
        tok = m.group(0)
        if tok.startswith("**"):
            runs.append((tok[2:-2], True, False))
        elif tok.startswith("`"):
            runs.append((tok[1:-1], False, False))
        else:
            runs.append((tok[1:-1], False, True))
        pos = m.end()
    if pos < len(text):
        runs.append((text[pos:], False, False))
    return runs or [(text, False, False)]


def create_docx(path, a):
    from docx import Document

    doc = Document()
    title = a.get("title")
    if title:
        doc.add_heading(str(title), level=0)

    content = a.get("content") or ""
    for kind, text in md_to_plain_blocks(content):
        if kind.startswith("h"):
            doc.add_heading(text, level=min(int(kind[1:]), 4))
        elif kind == "li":
            doc.add_paragraph(text, style="List Bullet")
        else:
            p = doc.add_paragraph()
            for seg, bold, italic in md_inline_runs(text):
                r = p.add_run(seg)
                r.bold = bold
                r.italic = italic

    # 可选：直接给表格
    for t in a.get("tables") or []:
        headers = t.get("headers") or []
        rows = t.get("rows") or []
        ncol = len(headers) or (len(rows[0]) if rows else 0)
        if not ncol:
            continue
        tbl = doc.add_table(rows=1, cols=ncol)
        tbl.style = "Table Grid"
        for i, h in enumerate(headers):
            tbl.rows[0].cells[i].text = str(h)
        for row in rows:
            cells = tbl.add_row().cells
            for i in range(ncol):
                cells[i].text = str(row[i]) if i < len(row) else ""

    ensure_dir(path)
    doc.save(path)
    return {"format": "docx", "paragraphs": len(doc.paragraphs), "tables": len(doc.tables)}


def create_xlsx(path, a):
    import openpyxl
    from openpyxl.styles import Font

    wb = openpyxl.Workbook()
    # 默认有一张空表；有 sheets 时删掉它
    sheets = a.get("sheets")
    if sheets:
        wb.remove(wb.active)
    else:
        ws = wb.active
        ws.title = a.get("sheet_name") or "Sheet1"
        headers = a.get("headers") or []
        rows = a.get("rows") or []
        if headers:
            ws.append([str(h) for h in headers])
            for c in ws[1]:
                c.font = Font(bold=True)
        for r in rows:
            ws.append(list(r))
        sheets = []

    for s in sheets:
        ws = wb.create_sheet(title=str(s.get("name") or "Sheet"))
        headers = s.get("headers") or []
        rows = s.get("rows") or []
        if headers:
            ws.append([str(h) for h in headers])
            for c in ws[1]:
                c.font = Font(bold=True)
        for r in rows:
            if isinstance(r, dict):
                r = list(r.values())
            ws.append(list(r))

    ensure_dir(path)
    wb.save(path)
    return {"format": "xlsx", "sheetnames": wb.sheetnames}


def create_pptx(path, a):
    from pptx import Presentation
    from pptx.util import Inches, Pt

    prs = Presentation()
    title = a.get("title") or ""
    content = a.get("content") or ""

    # 首页：标题版式
    slide = prs.slides.add_slide(prs.slide_layouts[0])
    slide.shapes.title.text = str(title) if title else "Untitled"
    try:
        slide.placeholders[1].text = a.get("subtitle") or ""
    except Exception:
        pass

    # 正文：按 ## 切分成多页
    sections = re.split(r"^##\s+", content, flags=re.M) if content else []
    if sections and sections[0].strip():
        # 第一个 ## 之前的内容并到首页副标题
        pass
    for sec in sections:
        if not sec.strip():
            continue
        lines = sec.splitlines()
        head = lines[0].strip()
        body = "\n".join(lines[1:]).strip()
        s = prs.slides.add_slide(prs.slide_layouts[1])
        s.shapes.title.text = head
        if body:
            tf = s.placeholders[1].text_frame
            tf.text = body.splitlines()[0][:200]
            for extra in body.splitlines()[1:]:
                p = tf.add_paragraph()
                p.text = extra[:200]

    if not sections and content:
        s = prs.slides.add_slide(prs.slide_layouts[1])
        s.shapes.title.text = title or "内容"
        tf = s.placeholders[1].text_frame
        lines = content.splitlines()
        tf.text = lines[0][:200] if lines else ""
        for extra in lines[1:]:
            p = tf.add_paragraph()
            p.text = extra[:200]

    ensure_dir(path)
    prs.save(path)
    return {"format": "pptx", "slides": len(prs.slides.__iter__.__self__._sldIdLst) if hasattr(prs.slides, "__iter__") else len(list(prs.slides))}


def create_markdown(path, a):
    """Markdown 直接落盘；给了 title 就补一级标题。"""
    content = a.get("content") or ""
    title = a.get("title")
    text = ""
    if title and not content.lstrip().startswith("# "):
        text += "# %s\n\n" % title
    text += content
    ensure_dir(path)
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)
    return {"format": "md", "bytes": len(text.encode("utf-8"))}


def create_csv(path, a):
    import csv

    ensure_dir(path)
    headers = a.get("headers") or []
    rows = a.get("rows") or []
    # 也支持 sheets[0]
    if not headers and not rows and a.get("sheets"):
        s0 = a["sheets"][0]
        headers = s0.get("headers") or []
        rows = s0.get("rows") or []
    with open(path, "w", encoding="utf-8", newline="") as f:
        w = csv.writer(f)
        if headers:
            w.writerow(headers)
        for r in rows:
            w.writerow(list(r) if not isinstance(r, dict) else list(r.values()))
    return {"format": "csv", "rows": len(rows) + (1 if headers else 0)}


def op_create(a):
    path = a.get("file") or ""
    if not path:
        fail("create 需要 file 参数", "传入要创建的文件绝对路径（扩展名决定格式）")
    e = ext_of(path)
    if e not in FORMATS:
        fail("不支持的创建格式: .%s" % e,
             "支持的扩展名: " + ", ".join(sorted(FORMATS.keys())))
    exists = os.path.exists(path)
    if exists and not a.get("overwrite"):
        fail("文件已存在: %s" % path,
             "传 overwrite=true 覆盖，或换一个文件名",
             path=path, exists=True)
    if exists:
        bk = backup(path)
    else:
        bk = None

    try:
        if e == "docx":
            info = create_docx(path, a)
        elif e == "xlsx":
            info = create_xlsx(path, a)
        elif e == "pptx":
            info = create_pptx(path, a)
        elif e == "md":
            info = create_markdown(path, a)
        elif e == "csv":
            info = create_csv(path, a)
        elif e == "txt":
            ensure_dir(path)
            with open(path, "w", encoding="utf-8") as f:
                f.write((a.get("content") or ""))
            info = {"format": "txt"}
        else:
            fail("暂不支持创建 .%s" % e, "支持: docx, xlsx, pptx, md, csv, txt")
    except Exception as ex:
        fail("创建失败 %s: %s" % (type(ex).__name__, ex),
             "检查参数（content/sheets/rows 结构）是否正确")

    info.update({"ok": True, "file": path, "created": not exists,
                 "overwritten": exists, "backup": bk,
                 "bytes": os.path.getsize(path)})
    info["summary"] = "已%s %s（%s，%d 字节）" % (
        "覆盖" if exists else "创建", path, info.get("format"), info["bytes"])
    emit(info)


def op_create_from_template(a):
    """复制模板再套用替换。"""
    src = a.get("template") or a.get("source") or ""
    dst = a.get("file") or a.get("target_file") or ""
    if not src or not dst:
        fail("create_from_template 需要 template 与 file 参数",
             '示例: {"template":"/path/a.docx","file":"/path/b.docx","replacements":{"旧":"新"}}')
    if not os.path.exists(src):
        fail("模板不存在: %s" % src, "检查 template 路径")
    if os.path.exists(dst) and not a.get("overwrite"):
        fail("目标已存在: %s" % dst, "传 overwrite=true 覆盖")
    ensure_dir(dst)
    try:
        shutil.copy2(src, dst)
    except Exception as ex:
        fail("复制模板失败: %s" % ex, "检查权限与磁盘空间")

    reps = a.get("replacements") or {}
    if a.get("target") and "replacement" in a:
        reps = {a["target"]: a.get("replacement", "")}

    count = 0
    if reps:
        e = ext_of(dst)
        if e == "docx":
            from docx import Document
            doc = Document(dst)

            def sub_paras(paras):
                nonlocal count
                for p in paras:
                    for k, v in reps.items():
                        if k in p.text:
                            count += p.text.count(k)
                            nt = p.text
                            for kk, vv in reps.items():
                                nt = nt.replace(kk, str(vv))
                            if p.runs:
                                p.runs[0].text = nt
                                for r in p.runs[1:]:
                                    r.text = ""
                            else:
                                p.text = nt

            def sub_tables(tables):
                for t in tables:
                    for row in t.rows:
                        for c in row.cells:
                            sub_paras(c.paragraphs)
                            sub_tables(c.tables)

            sub_paras(doc.paragraphs)
            sub_tables(doc.tables)
            if count:
                save_atomic(lambda p: doc.save(p), dst)
        elif e in ("xlsx", "xlsm"):
            import openpyxl
            wb = openpyxl.load_workbook(dst)
            for ws in wb.worksheets:
                for row in ws.iter_rows():
                    for c in row:
                        if isinstance(c.value, str):
                            for k, v in reps.items():
                                if k in c.value:
                                    count += c.value.count(k)
                                    c.value = c.value.replace(k, str(v))
            if count:
                save_atomic(lambda p: wb.save(p), dst)
        else:
            # 文本类直接替换
            with open(dst, "r", encoding="utf-8", errors="replace") as f:
                t = f.read()
            for k, v in reps.items():
                count += t.count(k)
                t = t.replace(k, str(v))
            if count:
                with open(dst, "w", encoding="utf-8") as f:
                    f.write(t)

    emit({"ok": True, "file": dst, "template": src, "replacements_applied": count,
          "summary": "已从模板 %s 创建 %s，替换 %d 处" % (src, dst, count)})


# ---------------------------------------------------------------- edit

def edit_docx(path, op, a):
    """docx 编辑。返回 (count, extra)"""
    from docx import Document
    from docx.shared import Pt, RGBColor
    from docx.enum.text import WD_ALIGN_PARAGRAPH

    doc = Document(path)
    target = a.get("target", "")
    repl = a.get("replacement", "")
    cs = bool(a.get("case_sensitive", True))
    rx = bool(a.get("regex", False))
    nth = a.get("nth")

    def all_paragraphs():
        for p in doc.paragraphs:
            yield p
        for t in doc.tables:
            for row in t.rows:
                for c in row.cells:
                    for p in c.paragraphs:
                        yield p
        for section in doc.sections:
            for part in (section.header, section.footer):
                try:
                    for p in part.paragraphs:
                        yield p
                except Exception:
                    pass

    if op == "replace_text":
        if not target:
            fail("replace_text 需要 target 参数", "传入要查找的文本")
        count = 0
        for p in all_paragraphs():
            if match_text(p.text, target, cs, rx) or match_text(p.text, target, cs, rx) == 0 and rx:
                new, n = apply_text(p.text, target, repl, cs, rx, nth)
                if n:
                    count += n
                    if p.runs:
                        p.runs[0].text = new
                        for r in p.runs[1:]:
                            r.text = ""
                    else:
                        p.text = new
        if count:
            save_atomic(lambda p: doc.save(p), path)
        return count, {}

    if op in ("insert_paragraph", "append_paragraph"):
        text = a.get("value") or a.get("text") or ""
        idx = a.get("index")
        if op == "append_paragraph" or idx is None:
            doc.add_paragraph(text)
            count = 1
        else:
            # 插到第 index 段之前
            idx = int(idx)
            paras = doc.paragraphs
            if idx < 0 or idx >= len(paras):
                fail("index 越界: %d（文档共 %d 段）" % (idx, len(paras)),
                     "index 从 0 开始，最大 %d" % (len(paras) - 1))
            ref = paras[idx]
            newp = ref.insert_paragraph_before(text)
            count = 1
        save_atomic(lambda p: doc.save(p), path)
        return count, {}

    if op == "insert_table":
        headers = a.get("headers") or []
        rows = a.get("rows") or []
        data = ([headers] if headers else []) + [list(r) if not isinstance(r, dict) else list(r.values()) for r in rows]
        if not data:
            fail("insert_table 需要 headers 或 rows", '示例 {"headers":["A","B"],"rows":[[1,2]]}')
        ncol = max(len(r) for r in data)
        t = doc.add_table(rows=0, cols=ncol)
        t.style = "Table Grid"
        for r in data:
            cells = t.add_row().cells
            for i in range(ncol):
                cells[i].text = str(r[i]) if i < len(r) else ""
        save_atomic(lambda p: doc.save(p), path)
        return len(data), {}

    if op == "set_style":
        # 对含 target 的段落设置样式；target 为空则作用于全文
        size = a.get("size")
        bold = a.get("bold")
        italic = a.get("italic")
        color = a.get("color")
        align = a.get("align")
        n = 0
        for p in all_paragraphs():
            if target and target not in p.text:
                continue
            if align in ("left", "center", "right", "justify"):
                p.alignment = {"left": WD_ALIGN_PARAGRAPH.LEFT,
                               "center": WD_ALIGN_PARAGRAPH.CENTER,
                               "right": WD_ALIGN_PARAGRAPH.RIGHT,
                               "justify": WD_ALIGN_PARAGRAPH.JUSTIFY}[align]
            for r in p.runs:
                if size is not None:
                    r.font.size = Pt(float(size))
                if bold is not None:
                    r.font.bold = bool(bold)
                if italic is not None:
                    r.font.italic = bool(italic)
                if color:
                    try:
                        r.font.color.rgb = RGBColor.from_string(str(color).lstrip("#").upper())
                    except Exception:
                        pass
            n += 1
        if n:
            save_atomic(lambda p: doc.save(p), path)
        return n, {}

    if op == "insert_image":
        img = a.get("image") or a.get("value") or ""
        if not img or not os.path.exists(img):
            fail("insert_image 需要 image 参数且文件必须存在: %s" % img,
                 '示例 {"image":"/path/a.png","width_inch":4}')
        w = a.get("width_inch")
        try:
            if w:
                doc.add_picture(img, width=__import__("docx").shared.Inches(float(w)))
            else:
                doc.add_picture(img)
        except Exception as ex:
            fail("插入图片失败: %s" % ex, "确认是 png/jpg 等支持格式")
        save_atomic(lambda p: doc.save(p), path)
        return 1, {}

    if op == "merge_cells":
        table_idx = int(a.get("table", 0))
        r1, c1 = int(a.get("row", 1)), int(a.get("col", 1))
        r2 = int(a.get("row2", r1))
        c2 = int(a.get("col2", c1))
        if table_idx >= len(doc.tables):
            fail("表格序号越界: %d（文档共 %d 张表）" % (table_idx, len(doc.tables)),
                 "table 从 0 开始")
        t = doc.tables[table_idx]
        try:
            cell_a = t.cell(r1 - 1, c1 - 1)
            cell_b = t.cell(r2 - 1, c2 - 1)
            cell_a.merge(cell_b)
        except Exception as ex:
            fail("合并单元格失败: %s" % ex, "检查 row/col/row2/col2 是否越界")
        save_atomic(lambda p: doc.save(p), path)
        return 1, {}

    fail("docx 不支持该操作: %s" % op,
         "docx 支持: replace_text, insert_paragraph, append_paragraph, set_style, "
         "insert_image, insert_table, merge_cells")


def edit_xlsx(path, op, a):
    import openpyxl
    from openpyxl.styles import Font, Alignment, PatternFill

    wb = openpyxl.load_workbook(path)
    sheet = a.get("sheet")
    ws = wb[sheet] if (sheet and sheet in wb.sheetnames) else wb.active
    target = a.get("target", "")
    repl = a.get("replacement", "")
    cs = bool(a.get("case_sensitive", True))
    rx = bool(a.get("regex", False))
    nth = a.get("nth")

    if op == "replace_text":
        if not target:
            fail("replace_text 需要 target 参数", "传入要查找的文本")
        count = 0
        sheets = [wb[sheet]] if (sheet and sheet in wb.sheetnames) else wb.worksheets
        for s in sheets:
            for row in s.iter_rows():
                for c in row:
                    if isinstance(c.value, str):
                        new, n = apply_text(c.value, target, repl, cs, rx, nth)
                        if n:
                            count += n
                            c.value = new
        if count:
            save_atomic(lambda p: wb.save(p), path)
        return count, {}

    if op == "set_cell":
        if a.get("row") is None or a.get("col") is None:
            fail("set_cell 需要 row / col 参数", '示例 {"row":1,"col":1,"value":"x"}')
        ws.cell(row=int(a["row"]), column=int(a["col"]), value=a.get("value", ""))
        save_atomic(lambda p: wb.save(p), path)
        return 1, {}

    if op == "insert_row":
        if a.get("row") is None:
            fail("insert_row 需要 row 参数", '示例 {"row":2}（在第 2 行前插入）')
        ws.insert_rows(int(a["row"]))
        save_atomic(lambda p: wb.save(p), path)
        return 1, {}

    if op == "delete_row":
        if a.get("row") is None:
            fail("delete_row 需要 row 参数", '示例 {"row":3}')
        n = int(a.get("count") or 1)
        ws.delete_rows(int(a["row"]), n)
        save_atomic(lambda p: wb.save(p), path)
        return n, {}

    if op == "add_sheet":
        name = a.get("value") or a.get("name") or ""
        if not name:
            fail("add_sheet 需要 value（新表名）", '示例 {"value":"Sheet2"}')
        if name in wb.sheetnames:
            fail("工作表已存在: %s" % name, "换一个名字")
        wb.create_sheet(title=name)
        save_atomic(lambda p: wb.save(p), path)
        return 1, {"sheetnames": wb.sheetnames}

    if op == "delete_sheet":
        name = a.get("value") or a.get("name") or sheet
        if not name or name not in wb.sheetnames:
            fail("要删除的工作表不存在: %s" % name,
                 "现有: " + ", ".join(wb.sheetnames))
        if len(wb.sheetnames) <= 1:
            fail("不能删除最后一张工作表", "Excel 要求至少保留一张表")
        del wb[name]
        save_atomic(lambda p: wb.save(p), path)
        return 1, {"sheetnames": wb.sheetnames}

    if op == "rename_sheet":
        old = sheet or a.get("old_name")
        new = a.get("value") or a.get("new_name")
        if not old or old not in wb.sheetnames:
            fail("原工作表不存在: %s" % old, "现有: " + ", ".join(wb.sheetnames))
        if not new:
            fail("rename_sheet 需要 value（新名字）", '示例 {"sheet":"Sheet1","value":"汇总"}')
        wb[old].title = new
        save_atomic(lambda p: wb.save(p), path)
        return 1, {"sheetnames": wb.sheetnames}

    if op == "set_style":
        row = a.get("row")
        col = a.get("col")
        if row is None or col is None:
            fail("xlsx set_style 需要 row/col", '示例 {"row":1,"col":1,"bold":true}')
        cell = ws.cell(row=int(row), column=int(col))
        if a.get("bold") is not None:
            cell.font = Font(bold=bool(a["bold"]), size=float(a["size"]) if a.get("size") else cell.font.size,
                             color=str(a["color"]).lstrip("#").upper() if a.get("color") else cell.font.color)
        elif a.get("size") is not None:
            cell.font = Font(bold=cell.font.bold, size=float(a["size"]))
        elif a.get("color"):
            cell.font = Font(bold=cell.font.bold, size=cell.font.size,
                             color=str(a["color"]).lstrip("#").upper())
        if a.get("align"):
            cell.alignment = Alignment(horizontal=str(a["align"]))
        if a.get("bg_color"):
            cell.fill = PatternFill(start_color=str(a["bg_color"]).lstrip("#").upper(),
                                    end_color=str(a["bg_color"]).lstrip("#").upper(), fill_type="solid")
        save_atomic(lambda p: wb.save(p), path)
        return 1, {}

    if op == "merge_cells":
        r1, c1 = int(a.get("row", 1)), int(a.get("col", 1))
        r2 = int(a.get("row2", r1))
        c2 = int(a.get("col2", c1))
        ws.merge_cells(start_row=r1, start_column=c1, end_row=r2, end_column=c2)
        save_atomic(lambda p: wb.save(p), path)
        return 1, {}

    if op == "insert_row" and a.get("row") is None:
        fail("insert_row 需要 row 参数", "")

    fail("xlsx 不支持该操作: %s" % op,
         "xlsx 支持: replace_text, set_cell, insert_row, delete_row, set_style, "
         "merge_cells, add_sheet, delete_sheet, rename_sheet")


def edit_pptx(path, op, a):
    from pptx import Presentation
    from pptx.util import Pt, Inches

    prs = Presentation(path)
    target = a.get("target", "")
    repl = a.get("replacement", "")
    cs = bool(a.get("case_sensitive", True))
    rx = bool(a.get("regex", False))
    nth = a.get("nth")

    def text_frames():
        for slide in prs.slides:
            for sh in slide.shapes:
                if getattr(sh, "has_text_frame", False) and sh.has_text_frame:
                    yield sh.text_frame
                if getattr(sh, "has_table", False) and sh.has_table:
                    for row in sh.table.rows:
                        for c in row.cells:
                            yield c.text_frame

    if op == "replace_text":
        if not target:
            fail("replace_text 需要 target 参数", "传入要查找的文本")
        count = 0
        for tf in text_frames():
            for para in tf.paragraphs:
                new, n = apply_text(para.text, target, repl, cs, rx, nth)
                if n:
                    count += n
                    if para.runs:
                        para.runs[0].text = new
                        for r in para.runs[1:]:
                            r.text = ""
                    else:
                        para.text = new
        if count:
            save_atomic(lambda p: prs.save(p), path)
        return count, {}

    if op == "append_paragraph":
        text = a.get("value") or a.get("text") or ""
        slide_no = int(a.get("slide") or 1)
        slides = list(prs.slides)
        if slide_no < 1 or slide_no > len(slides):
            fail("slide 越界: %d（共 %d 页）" % (slide_no, len(slides)),
                 "slide 从 1 开始")
        slide = slides[slide_no - 1]
        # 追加到一个新文本框
        from pptx.util import Emu
        left = Inches(0.5)
        top = Inches(1.5)
        width = prs.slide_width - Inches(1)
        height = Inches(1)
        tb = slide.shapes.add_textbox(left, top, width, height)
        tb.text_frame.text = text
        save_atomic(lambda p: prs.save(p), path)
        return 1, {}

    if op == "set_cell":
        if a.get("row") is None or a.get("col") is None:
            fail("set_cell 需要 row / col 参数", "row/col 从 1 开始，作用于第一张表")
        for slide in prs.slides:
            for sh in slide.shapes:
                if getattr(sh, "has_table", False) and sh.has_table:
                    cell = sh.table.cell(int(a["row"]) - 1, int(a["col"]) - 1)
                    tf = cell.text_frame
                    if tf.paragraphs and tf.paragraphs[0].runs:
                        tf.paragraphs[0].runs[0].text = a.get("value", "")
                        for r in tf.paragraphs[0].runs[1:]:
                            r.text = ""
                    else:
                        tf.text = a.get("value", "")
                    save_atomic(lambda p: prs.save(p), path)
                    return 1, {}
        fail("pptx 中未找到表格", "set_cell 需要页面里已有表格")

    if op == "insert_row":
        # 语义降级：python-pptx 无插入行 API，这里是「在表格末尾追加一行」
        from copy import deepcopy
        tbl = None
        for slide in prs.slides:
            for sh in slide.shapes:
                if getattr(sh, "has_table", False) and sh.has_table:
                    tbl = sh.table
                    break
            if tbl is not None:
                break
        if tbl is None:
            fail("pptx 中未找到表格", "insert_row 需要页面里已有表格")
        tr = deepcopy(tbl._tbl.tr_lst[-1])
        tbl._tbl.append(tr)
        for tc in tr.tc_lst:
            for p in tc.findall('.//{http://schemas.openxmlformats.org/drawingml/2006/main}p'):
                for r in p.findall('{http://schemas.openxmlformats.org/drawingml/2006/main}r'):
                    p.remove(r)
        save_atomic(lambda p: prs.save(p), path)
        return 1, {"degraded": "pptx 无插入行 API，已降级为在表格末尾追加一行"}

    if op == "insert_image":
        img = a.get("image") or a.get("value") or ""
        if not img or not os.path.exists(img):
            fail("insert_image 需要 image 参数且文件必须存在: %s" % img,
                 '示例 {"image":"/path/a.png","slide":1}')
        slide_no = int(a.get("slide") or 1)
        slides = list(prs.slides)
        if slide_no < 1 or slide_no > len(slides):
            fail("slide 越界: %d（共 %d 页）" % (slide_no, len(slides)), "slide 从 1 开始")
        slides[slide_no - 1].shapes.add_picture(
            img, Inches(float(a.get("left_inch", 1))), Inches(float(a.get("top_inch", 1))),
            width=Inches(float(a["width_inch"])) if a.get("width_inch") else None)
        save_atomic(lambda p: prs.save(p), path)
        return 1, {}

    if op in ("merge_cells", "set_style"):
        fail("pptx 不支持 %s" % op,
             "merge_cells 仅支持 xlsx/docx；pptx 表格无合并 API",
             unsupported_op=op, format="pptx")

    fail("pptx 不支持该操作: %s" % op,
         "pptx 支持: replace_text, append_paragraph, set_cell, insert_row(追加行), insert_image")


def op_edit(a):
    path = a.get("file") or ""
    if not path:
        fail("edit 需要 file 参数", "传入要编辑的文件绝对路径")
    op = a.get("operation") or ""
    if not op:
        fail("edit 需要 operation 参数",
             "支持: replace_text, set_cell, insert_row, delete_row, "
             "insert_paragraph, append_paragraph, set_style, insert_image, "
             "insert_table, merge_cells, add_sheet, delete_sheet, rename_sheet")
    if not os.path.exists(path):
        fail("文件不存在: %s" % path,
             "编辑只能作用于已存在文件；要新建请用 create_document",
             path=path, exists=False)
    if a.get("dry_run"):
        emit({"ok": True, "dry_run": True, "file": path, "operation": op,
              "summary": "dry_run：未做任何修改"})

    backup_path = backup(path)
    e = ext_of(path)
    try:
        if e == "docx":
            count, extra = edit_docx(path, op, a)
        elif e in ("xlsx", "xlsm"):
            count, extra = edit_xlsx(path, op, a)
        elif e == "pptx":
            count, extra = edit_pptx(path, op, a)
        else:
            fail("不支持编辑的格式: .%s" % e,
                 "支持: .docx / .xlsx / .xlsm / .pptx")
    except SystemExit:
        raise
    except Exception as ex:
        fail("%s: %s" % (type(ex).__name__, ex),
             "检查 operation 与参数组合是否匹配")

    r = {"ok": True, "file": path, "operation": op, "count": count,
         "backup": backup_path}
    r.update(extra)
    msg = "编辑完成，已保存到原文件: %s" % path
    if count:
        msg += "\n共处理 %d 处" % count
    else:
        msg += "\n未匹配到任何内容（0 处）"
    if extra.get("degraded"):
        msg += "\n注意: " + extra["degraded"]
    r["summary"] = msg
    r["content"] = msg  # 兼容 v1 调用方读取 content
    emit(r)


# ---------------------------------------------------------------- convert

def op_convert(a):
    src = a.get("file") or ""
    to = (a.get("to") or "").lower().lstrip(".")
    if not src or not to:
        fail("convert 需要 file 与 to 参数",
             '示例: {"file":"/p/a.md","to":"docx"}')
    if not os.path.exists(src):
        fail("源文件不存在: %s" % src, "检查路径")
    dst = a.get("output") or (os.path.splitext(src)[0] + "." + to)
    if os.path.exists(dst) and not a.get("overwrite"):
        fail("输出文件已存在: %s" % dst, "传 overwrite=true 覆盖")
    ensure_dir(dst)
    se, de = ext_of(src), to

    tool = None
    if se == "md" and de in ("docx", "html", "pdf", "pptx"):
        tool = "pandoc"
    elif de in ("pdf",) or (se in ("docx", "xlsx", "pptx") and de in ("csv", "txt", "html")):
        tool = "soffice"

    # xlsx → csv 用 openpyxl 更干净
    if se in ("xlsx", "xlsm") and de == "csv":
        require_dep("openpyxl", "openpyxl", "Excel 转换", "xlsx")
        import openpyxl
        import csv as _csv
        wb = openpyxl.load_workbook(src, data_only=True)
        sheet = a.get("sheet")
        ws = wb[sheet] if (sheet and sheet in wb.sheetnames) else wb.active
        # 多表时按 表名.csv 输出
        if not sheet and len(wb.sheetnames) > 1 and not a.get("output"):
            outs = []
            base = os.path.splitext(src)[0]
            for name in wb.sheetnames:
                p = "%s.%s.csv" % (base, name)
                with open(p, "w", encoding="utf-8", newline="") as f:
                    w = _csv.writer(f)
                    for row in wb[name].iter_rows(values_only=True):
                        w.writerow(["" if v is None else v for v in row])
                outs.append(p)
            emit({"ok": True, "file": src, "to": "csv", "outputs": outs,
                  "summary": "已导出 %d 张工作表为 CSV: %s" % (len(outs), ", ".join(outs))})
        with open(dst, "w", encoding="utf-8", newline="") as f:
            w = _csv.writer(f)
            for row in ws.iter_rows(values_only=True):
                w.writerow(["" if v is None else v for v in row])
        emit({"ok": True, "file": src, "output": dst,
              "summary": "已导出 %s 的 %s 表到 %s" % (src, ws.title, dst)})

    # pptx → txt / md 用 python-pptx 直接抽
    if se == "pptx" and de in ("txt", "md"):
        require_dep("pptx", "python-pptx", "PPTX 读取", "pptx")
        r = read_pptx(src)
        lines = []
        for s in r["slides"]:
            lines.append("# 第 %d 页" % s["slide"])
            lines.extend(s["texts"])
            for t in s["tables"]:
                for row in t:
                    lines.append(" | ".join(row))
            lines.append("")
        with open(dst, "w", encoding="utf-8") as f:
            f.write("\n".join(lines))
        emit({"ok": True, "file": src, "output": dst, "slides": len(r["slides"]),
              "summary": "已抽取 %d 页到 %s" % (len(r["slides"]), dst)})

    if tool and not which(tool):
        fail("缺少外部工具 %s，无法完成 .%s → .%s 转换" % (tool, se, de),
             "请安装: sudo apt install %s" % ("pandoc" if tool == "pandoc" else "libreoffice"),
             missing=[tool])

    try:
        if tool == "pandoc":
            cmd = ["pandoc", src, "-o", dst]
        elif tool == "soffice":
            outdir = os.path.dirname(os.path.abspath(dst)) or "."
            cmd = ["soffice", "--headless", "--convert-to", de, "--outdir", outdir, src]
        else:
            fail("不支持的转换组合: .%s → .%s" % (se, de),
                 "支持: md→docx/html/pdf、xlsx→csv、pptx→txt/md、office→pdf")
        p = subprocess.run(cmd, capture_output=True, timeout=int(a.get("timeout") or 120))
        if p.returncode != 0:
            fail("%s 转换失败（退出码 %d）" % (tool, p.returncode),
                 (p.stderr or b"").decode("utf-8", "replace")[:300],
                 cmd=" ".join(cmd))
    except subprocess.TimeoutExpired:
        fail("转换超时", "文件过大或 soffice 首次启动较慢，可稍后重试")

    if not os.path.exists(dst):
        # soffice 可能改了扩展名
        alt = os.path.splitext(src)[0] + "." + de
        if os.path.exists(alt):
            dst = alt
        else:
            fail("转换命令已执行但未找到输出文件",
                 "检查 %s 是否支持该格式组合" % tool, cmd=" ".join(cmd))
    emit({"ok": True, "file": src, "output": dst, "tool": tool,
          "bytes": os.path.getsize(dst),
          "summary": "已用 %s 将 %s 转为 %s" % (tool, src, dst)})


# ---------------------------------------------------------------- check

def op_check(a):
    """独立可调用的自检：返回每个库的可用性 + 支持格式矩阵。"""
    import sys as _sys
    libs = []
    for mod, pipname, purpose in DEPS:
        ok = dep_ok(mod)
        ver = None
        if ok:
            try:
                m = __import__(mod)
                ver = getattr(m, "__version__", None)
                if ver is None and hasattr(m, "VERSION"):
                    ver = ".".join(str(x) for x in m.VERSION)
            except Exception:
                pass
        libs.append({"module": mod, "pip": pipname, "purpose": purpose,
                     "available": ok, "version": ver,
                     "install_cmd": None if ok else "%s -m pip install %s" % (_sys.executable, pipname)})

    bins = []
    for b, purpose in BINS:
        p = which(b)
        bins.append({"bin": b, "purpose": purpose, "available": bool(p), "path": p,
                     "install_cmd": None if p else "sudo apt install %s" % b})

    missing = [l["pip"] for l in libs if not l["available"]]
    missing_bins = [b["bin"] for b in bins if not b["available"]]

    # 支持矩阵：按实际依赖算「当前可用」还是「装包后可用」
    matrix = {}
    for fmt, ops in FORMATS.items():
        need = {"docx": "python-docx", "xlsx": "openpyxl", "xlsm": "openpyxl",
                "pptx": "python-pptx", "xls": "xlrd"}.get(fmt)
        usable = True
        if need and need in missing:
            usable = False
        matrix[fmt] = {"operations": ops, "usable_now": usable,
                       "requires": need or "-"}

    lines = ["依赖自检结果："]
    for l in libs:
        lines.append("  %-10s %s %s" % (l["module"],
                                        "OK" if l["available"] else "缺失",
                                        l["version"] or ""))
    for b in bins:
        lines.append("  %-10s %s %s" % (b["bin"], "OK" if b["available"] else "缺失",
                                        b["path"] or ""))
    lines.append("支持格式: " + ", ".join("%s(%s)" % (f, "可用" if m["usable_now"] else "缺依赖")
                                       for f, m in sorted(matrix.items())))
    if missing or missing_bins:
        lines.append("缺失项: " + ", ".join(missing + missing_bins))
        for l in libs:
            if not l["available"]:
                lines.append("  修复: " + l["install_cmd"])
        for b in bins:
            if not b["available"]:
                lines.append("  修复: " + b["install_cmd"])
    else:
        lines.append("全部依赖就绪。")

    emit({"ok": True, "all_ready": not (missing or missing_bins),
          "python": _sys.version.split()[0], "interpreter": _sys.executable,
          "libs": libs, "bins": bins, "formats": matrix,
          "missing": missing + missing_bins,
          "summary": "\n".join(lines)})


# ---------------------------------------------------------------- main

COMMANDS = {
    "read": op_read,
    "create": op_create,
    "create_from_template": op_create_from_template,
    "edit": op_edit,
    "convert": op_convert,
    "check": op_check,
}


def main():
    if len(sys.argv) < 2:
        fail("用法: edit_doc.py <command> [args_json]",
             "command ∈ " + ", ".join(sorted(COMMANDS)))
    cmd = sys.argv[1]
    try:
        args = json.loads(sys.argv[2]) if len(sys.argv) > 2 and sys.argv[2].strip() else {}
    except Exception as ex:
        fail("参数 JSON 解析失败: %s" % ex, "检查 JSON 引号转义")

    fn = COMMANDS.get(cmd)
    if fn is None:
        fail("未知命令: %s" % cmd, "支持: " + ", ".join(sorted(COMMANDS)))
    try:
        fn(args)
    except SystemExit:
        raise
    except Exception as ex:
        import traceback
        fail("%s: %s" % (type(ex).__name__, ex),
             traceback.format_exc()[-400:])


if __name__ == "__main__":
    main()
