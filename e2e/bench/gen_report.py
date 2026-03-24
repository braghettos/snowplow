#!/usr/bin/env python3
"""Generate Snowplow Cache v0.25.35 Performance & Correctness Report PDF."""

from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import getSampleStyleSheet, ParagraphStyle
from reportlab.lib.units import mm
from reportlab.lib.colors import HexColor
from reportlab.lib.enums import TA_CENTER, TA_LEFT
from reportlab.platypus import (
    SimpleDocTemplate, Paragraph, Spacer, Table, TableStyle, PageBreak
)

OUTPUT = "/Users/diegobraga/krateo/snowplow-cache/snowplow/e2e/bench/snowplow_cache_v0.25.35_report.pdf"

BLUE = HexColor("#1a56db")
GREEN = HexColor("#059669")
LIGHT_BLUE = HexColor("#eff6ff")
LIGHT_GREEN = HexColor("#ecfdf5")
LIGHT_GRAY = HexColor("#f3f4f6")
DARK = HexColor("#111827")
WHITE = HexColor("#ffffff")
RED = HexColor("#dc2626")
YELLOW = HexColor("#d97706")

styles = getSampleStyleSheet()
styles.add(ParagraphStyle("ReportTitle", parent=styles["Title"], fontSize=22, textColor=BLUE, spaceAfter=6))
styles.add(ParagraphStyle("Subtitle", parent=styles["Normal"], fontSize=11, textColor=HexColor("#6b7280"), spaceAfter=20))
styles.add(ParagraphStyle("SectionHead", parent=styles["Heading1"], fontSize=14, textColor=BLUE, spaceBefore=18, spaceAfter=8))
styles.add(ParagraphStyle("SubHead", parent=styles["Heading2"], fontSize=12, textColor=DARK, spaceBefore=12, spaceAfter=6))
styles.add(ParagraphStyle("Body", parent=styles["Normal"], fontSize=9.5, leading=13, spaceAfter=6))
styles.add(ParagraphStyle("BulletItem", parent=styles["Normal"], fontSize=9.5, leading=13, leftIndent=16, bulletIndent=6, spaceAfter=3))
styles.add(ParagraphStyle("CellText", parent=styles["Normal"], fontSize=8.5, leading=11))
styles.add(ParagraphStyle("CellBold", parent=styles["Normal"], fontSize=8.5, leading=11, fontName="Helvetica-Bold"))
styles.add(ParagraphStyle("StepNum", parent=styles["Normal"], fontSize=9.5, leading=13, leftIndent=16, spaceAfter=3))


def make_table(headers, rows, col_widths=None, highlight_last_col=False):
    data = [[Paragraph(f"<b>{h}</b>", styles["CellBold"]) for h in headers]]
    for row in rows:
        data.append([Paragraph(str(c), styles["CellText"]) for c in row])

    style_cmds = [
        ("BACKGROUND", (0, 0), (-1, 0), BLUE),
        ("TEXTCOLOR", (0, 0), (-1, 0), WHITE),
        ("FONTNAME", (0, 0), (-1, 0), "Helvetica-Bold"),
        ("FONTSIZE", (0, 0), (-1, 0), 9),
        ("BOTTOMPADDING", (0, 0), (-1, 0), 6),
        ("TOPPADDING", (0, 0), (-1, 0), 6),
        ("GRID", (0, 0), (-1, -1), 0.5, HexColor("#d1d5db")),
        ("ROWBACKGROUNDS", (0, 1), (-1, -1), [WHITE, LIGHT_GRAY]),
        ("VALIGN", (0, 0), (-1, -1), "MIDDLE"),
        ("LEFTPADDING", (0, 0), (-1, -1), 6),
        ("RIGHTPADDING", (0, 0), (-1, -1), 6),
        ("TOPPADDING", (0, 1), (-1, -1), 4),
        ("BOTTOMPADDING", (0, 1), (-1, -1), 4),
    ]

    t = Table(data, colWidths=col_widths, repeatRows=1)
    t.setStyle(TableStyle(style_cmds))
    return t


def build():
    doc = SimpleDocTemplate(
        OUTPUT, pagesize=A4,
        leftMargin=20*mm, rightMargin=20*mm,
        topMargin=20*mm, bottomMargin=20*mm,
    )
    story = []
    W = doc.width

    # Title
    story.append(Paragraph("Snowplow Cache v0.25.35", styles["ReportTitle"]))
    story.append(Paragraph("Performance &amp; Correctness Report", styles["ReportTitle"]))
    story.append(Paragraph("Date: 2026-03-22 | Cluster: GKE | Scale: 0 to 1200 compositions", styles["Subtitle"]))

    # Executive Summary
    story.append(Paragraph("Executive Summary", styles["SectionHead"]))
    story.append(Paragraph(
        "Snowplow cache v0.25.35 introduces API-level dependency tracking with cascading refresh "
        "for RESTAction resolution. This ensures widgets like piecharts and tables are correctly "
        "updated when underlying K8s resources change. All verification stages pass (except one "
        "timing race), and dashboard warm response stays flat at <b>53-67ms</b> regardless of scale.",
        styles["Body"],
    ))

    # Key Changes
    story.append(Paragraph("Key Changes (v0.25.32 to v0.25.35)", styles["SectionHead"]))
    for b in [
        "Per-resource dependency index for targeted L1 invalidation",
        "API-level dependency extraction from resolved RESTAction apiRequests",
        "Cascading refresh: RESTAction refresh triggers dependent widget refresh",
        "Fixed zero-state problem: piechart updates on ADD even when resolved with 0 compositions",
    ]:
        story.append(Paragraph(f"\u2022 {b}", styles["BulletItem"]))

    # Verification
    story.append(Paragraph("Cache Correctness Verification", styles["SectionHead"]))
    story.append(Paragraph(
        "Each stage verifies the composition count via three methods: snowplow API (piechart widget), "
        "browser UI (Playwright DOM inspection), and kubectl cluster count. All three must match.",
        styles["Body"],
    ))
    story.append(make_table(
        ["Stage", "Description", "Cache ON (api/ui/cluster)", "Cache OFF (api/ui/cluster)", "Result"],
        [
            ["S1", "Zero state (0 comp)", "0 / 0 / 0", "0 / 0 / 0", "PASS"],
            ["S2", "1 ns + compdef (0 comp)", "0 / 0 / 0", "0 / 0 / 0", "PASS"],
            ["S3", "20 bench ns (0 comp)", "0 / 0 / 0", "0 / 0 / 0", "PASS"],
            ["S4", "20 compositions", "20 / 20 / 20", "20 / 20 / 20", "PASS"],
            ["S5", "120 ns + ~303 comp", "304 / 304 / 303", "? / ? / 20", "TIMING RACE"],
            ["S6", "1189 compositions", "1189 / 1189 / 1189", "1200 / 1200 / 1200", "PASS"],
            ["S7", "Deleted 1 comp", "1189 / 1189 / 1189", "1199 / 1199 / 1199", "PASS"],
            ["S8", "Deleted 1 ns", "1189 / 1189 / 1189", "1189 / 1189 / 1189", "PASS"],
        ],
        col_widths=[W*0.06, W*0.22, W*0.22, W*0.22, W*0.14],
    ))
    story.append(Spacer(1, 4))
    story.append(Paragraph(
        "<i>S5 mismatch is a timing race: compositions were still being created during verification.</i>",
        styles["Body"],
    ))

    # Dashboard Performance
    story.append(Paragraph("Dashboard Performance", styles["SectionHead"]))
    story.append(make_table(
        ["Stage", "Description", "ON warm", "OFF warm", "Speedup"],
        [
            ["S1", "Zero state", "62ms", "3,083ms", "49.7x"],
            ["S2", "1 ns + compdef", "61ms", "3,122ms", "51.2x"],
            ["S3", "20 bench ns", "53ms", "15,991ms", "301.7x *"],
            ["S4", "20 compositions", "61ms", "3,441ms", "56.4x"],
            ["S5", "120 bench ns", "60ms", "3,458ms", "57.6x *"],
            ["S6", "1189 compositions", "67ms", "8,629ms", "128.8x"],
            ["S7", "Deleted 1 comp", "61ms", "8,515ms", "139.6x"],
            ["S8", "Deleted 1 ns", "63ms", "7,262ms", "115.3x"],
        ],
        col_widths=[W*0.08, W*0.25, W*0.15, W*0.15, W*0.15],
    ))
    story.append(Paragraph("<i>* = incomplete page load</i>", styles["Body"]))

    # Compositions Page Performance
    story.append(Paragraph("Compositions Page Performance", styles["SectionHead"]))
    story.append(make_table(
        ["Stage", "Description", "ON warm", "OFF warm", "Speedup"],
        [
            ["S1", "Zero state", "45ms", "2,486ms", "55.2x"],
            ["S2", "1 ns + compdef", "42ms", "3,161ms", "75.3x"],
            ["S3", "20 bench ns", "572ms", "2,768ms", "4.8x"],
            ["S4", "20 compositions", "68ms", "4,700ms", "69.1x"],
            ["S5", "120 bench ns", "64ms", "4,721ms", "73.8x"],
            ["S6", "1189 compositions", "2,682ms", "7,401ms", "2.8x"],
            ["S7", "Deleted 1 comp", "1,287ms", "7,038ms", "5.5x"],
            ["S8", "Deleted 1 ns", "73ms", "8,143ms", "111.5x"],
        ],
        col_widths=[W*0.08, W*0.25, W*0.15, W*0.15, W*0.15],
    ))

    # Regression Fix
    story.append(PageBreak())
    story.append(Paragraph("Regression Fix: S7/S8 (v0.25.31 to v0.25.35)", styles["SectionHead"]))
    story.append(Paragraph(
        "The original S7/S8 regression was caused by the DELETE handler nuking ALL 1,842 L1 keys "
        "when a single composition was deleted. The per-resource dependency index now targets only "
        "the 6-10 L1 keys that actually reference the deleted resource.",
        styles["Body"],
    ))
    story.append(make_table(
        ["Stage", "v0.25.31", "v0.25.35", "Improvement"],
        [
            ["S7 Dashboard (delete 1 comp)", "23,686ms", "61ms", "388x faster"],
            ["S8 Dashboard (delete 1 ns)", "6,019ms", "63ms", "96x faster"],
        ],
        col_widths=[W*0.32, W*0.18, W*0.18, W*0.18],
    ))

    # Test Duration
    story.append(Paragraph("Test Duration", styles["SectionHead"]))
    story.append(make_table(
        ["Phase", "Duration"],
        [
            ["Cache ON (deploy + measure)", "~57 min"],
            ["Cache OFF (cleanup + deploy + measure)", "~34 min"],
            ["Total", "~2 hours"],
        ],
        col_widths=[W*0.5, W*0.2],
    ))

    # Architecture
    story.append(Paragraph("Architecture: Dependency Chain", styles["SectionHead"]))
    story.append(Paragraph("When a composition is created or deleted:", styles["Body"]))
    steps = [
        "Informer fires event for composition GVR",
        "collectAffectedL1Keys looks up: GET dep, LIST ns dep, LIST cluster dep, API dep",
        "API dep finds compositions-list RESTAction L1 key",
        "compositions-list is refreshed via coalescing queue",
        "Cascading refresh finds piechart/table L1 keys that depend on compositions-list",
        "Piechart/table refreshed with correct count",
    ]
    for i, s in enumerate(steps, 1):
        story.append(Paragraph(f"<b>{i}.</b> {s}", styles["StepNum"]))

    # Redis Key Structure
    story.append(Paragraph("Redis Dependency Index Structure", styles["SubHead"]))
    keys = [
        ("<b>snowplow:l1dep:{gvr}:{ns}:{name}</b> - Per-resource GET dependency (specific resource to L1 key)",),
        ("<b>snowplow:l1dep:{gvr}:{ns}:</b> - Per-resource LIST dependency (namespaced list to L1 key)",),
        ("<b>snowplow:l1dep:{gvr}::</b> - Cluster-wide LIST dependency",),
        ("<b>snowplow:l1api:{gvrKey}</b> - API-level dependency (extracted from apiRequests)",),
        ("<b>snowplow:l1gvr:{gvrKey}</b> - GVR-level index (all L1 keys touching a GVR)",),
    ]
    for k in keys:
        story.append(Paragraph(f"\u2022 {k[0]}", styles["BulletItem"]))

    doc.build(story)
    print(f"PDF saved to {OUTPUT}")


if __name__ == "__main__":
    build()
