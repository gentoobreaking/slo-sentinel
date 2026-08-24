package main

// waste_cli.go（T024）：waste 候選清單的 CLI 入口——list / dismiss / resolve。
//
//	sentinel waste list [-config path] [-db path]
//	sentinel waste dismiss <resource_id> [-days N] [-reason "..."] [-config path]
//	sentinel waste resolve <resource_id> [-config path]
//
// dismiss/resolve 經 Tracker 生命週期寫回 SQLite，daemon 重啟後狀態一致；
// resolve 後的累積節省金額可直接查詢（list 尾端輸出）。

import (
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"slo-sentinel/config"
	"slo-sentinel/internal/store"
	"slo-sentinel/internal/waste"
)

func runWasteCLI(args []string) {
	if len(args) == 0 {
		wasteUsage()
		os.Exit(2)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		wasteList(rest)
	case "dismiss":
		wasteDismiss(rest)
	case "resolve":
		wasteResolve(rest)
	default:
		fmt.Fprintf(os.Stderr, "未知的子命令 %q\n", sub)
		wasteUsage()
		os.Exit(2)
	}
}

func wasteUsage() {
	fmt.Fprint(os.Stderr, `用法：
  sentinel waste list [-config path] [-db path]
  sentinel waste dismiss <resource_id> [-days N] [-reason "..."] [-config path] [-db path]
  sentinel waste resolve <resource_id> [-config path] [-db path]

說明：
  list     列出所有候選與生命週期狀態，尾端顯示已結案累積節省金額
  dismiss  暫不處理；-days 設復活期限天數（0 = 永久擱置）
  resolve  標記已處理，累積浪費金額計入節省統計
`)
}

// openWasteStore 解析 -config/-db 並開啟 SQLite。
func openWasteStore(fs *flag.FlagSet, args []string) *store.Store {
	cfgPath := fs.String("config", "", "設定檔路徑")
	dbPath := fs.String("db", "", "SQLite 路徑（覆寫 config）")
	fs.Parse(args)
	path := *dbPath
	if path == "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "❌ 設定載入失敗：%v\n", err)
			os.Exit(1)
		}
		path = cfg.DBPath
	}
	st, err := store.Open(resolveDBPath(path))
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 開啟資料庫失敗：%v\n", err)
		os.Exit(1)
	}
	return st
}

func loadTracker(st *store.Store) *waste.Tracker {
	tr := waste.NewLiveTracker()
	entries, err := st.AllWasteEntries()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 讀取候選清單失敗：%v\n", err)
		os.Exit(1)
	}
	tr.Restore(entriesFromStore(entries))
	return tr
}

// entriesFromStore / entryToStore：store 持久化形態 ↔ Tracker 條目的轉換。
func entriesFromStore(rows []store.WasteEntry) []waste.Entry {
	out := make([]waste.Entry, 0, len(rows))
	for _, e := range rows {
		out = append(out, waste.Entry{
			SensorID: e.SensorID, ResourceID: e.ResourceID, Reason: e.Reason,
			State: waste.Lifecycle(e.State), FirstSeen: e.FirstSeen,
			LastNotified: e.LastNotified, Renotify: e.Renotify,
			WasteUSDPerDay: e.WasteUSDPerDay, TotalWasteUSD: e.TotalWasteUSD,
			DismissReason: e.DismissReason, DismissUntil: e.DismissUntil,
		})
	}
	return out
}

func entryToStore(e waste.Entry) store.WasteEntry {
	return store.WasteEntry{
		SensorID: e.SensorID, ResourceID: e.ResourceID, Reason: e.Reason,
		State: string(e.State), FirstSeen: e.FirstSeen, LastNotified: e.LastNotified,
		Renotify: e.Renotify, WasteUSDPerDay: e.WasteUSDPerDay,
		TotalWasteUSD: e.TotalWasteUSD, DismissReason: e.DismissReason,
		DismissUntil: e.DismissUntil,
	}
}

// syncTracker 把 Tracker 全部條目寫回 store。
func syncTracker(st *store.Store, tr *waste.Tracker) {
	for _, e := range tr.Entries() {
		if err := st.SetWasteEntry(entryToStore(e)); err != nil {
			fmt.Fprintf(os.Stderr, "❌ 寫入失敗（%s）：%v\n", e.ResourceID, err)
			os.Exit(1)
		}
	}
}

func wasteList(args []string) {
	fs := flag.NewFlagSet("waste list", flag.ExitOnError)
	st := openWasteStore(fs, args)
	defer st.Close()

	entries, err := st.AllWasteEntries()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 讀取候選清單失敗：%v\n", err)
		os.Exit(1)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SENSOR\tRESOURCE\tSTATE\tFIRST SEEN\tTOTAL WASTE\tNOTE\t")
	for _, e := range entries {
		note := ""
		switch e.State {
		case "dismissed":
			if !e.DismissUntil.IsZero() {
				note = fmt.Sprintf("復活於 %s", e.DismissUntil.Format("2006-01-02"))
				if e.DismissReason != "" {
					note += "：" + e.DismissReason
				}
			} else if e.DismissReason != "" {
				note = e.DismissReason
			}
		case "resolved":
			note = "已結案"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t$%.2f\t%s\t\n",
			e.SensorID, e.ResourceID, e.State,
			e.FirstSeen.Format("2006-01-02"), e.TotalWasteUSD, note)
	}
	tw.Flush()
	saving, err := st.ResolvedWasteSaving()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ 統計節省金額失敗：%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n已結案候選 %d 個，累積節省金額：$%.2f/月\n", countResolved(entries), saving)
}

func countResolved(entries []store.WasteEntry) int {
	n := 0
	for _, e := range entries {
		if e.State == "resolved" {
			n++
		}
	}
	return n
}

func wasteDismiss(args []string) {
	fs := flag.NewFlagSet("waste dismiss", flag.ExitOnError)
	days := fs.Int("days", 0, "暫緩天數（0 = 永久擱置）")
	reason := fs.String("reason", "", "暫緩原因")
	st := openWasteStore(fs, args)
	defer st.Close()
	ids := fs.Args()
	if len(ids) != 1 {
		fmt.Fprintln(os.Stderr, "用法：sentinel waste dismiss <resource_id> [-days N] [-reason ...]")
		os.Exit(2)
	}

	var until time.Time // 零值 = 不復活
	if *days > 0 {
		until = time.Now().AddDate(0, 0, *days)
	}
	tr := loadTracker(st)
	if err := tr.Dismiss(ids[0], *reason, until); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	syncTracker(st, tr)
	fmt.Printf("✅ %s 已暫緩", ids[0])
	if *days > 0 {
		fmt.Printf("，%s 復活", until.Format("2006-01-02"))
	}
	fmt.Println()
}

func wasteResolve(args []string) {
	fs := flag.NewFlagSet("waste resolve", flag.ExitOnError)
	st := openWasteStore(fs, args)
	defer st.Close()
	ids := fs.Args()
	if len(ids) != 1 {
		fmt.Fprintln(os.Stderr, "用法：sentinel waste resolve <resource_id>")
		os.Exit(2)
	}

	tr := loadTracker(st)
	before := tr.ResolvedSaving()
	if err := tr.Resolve(ids[0]); err != nil {
		fmt.Fprintf(os.Stderr, "❌ %v\n", err)
		os.Exit(1)
	}
	syncTracker(st, tr)
	fmt.Printf("✅ %s 已結案，本次計入節省 $%.2f；累積節省金額：$%.2f/月\n",
		ids[0], tr.ResolvedSaving()-before, tr.ResolvedSaving())
}
