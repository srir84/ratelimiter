// Package scripts holds the store's Lua scripts (DESIGN §0, §A4): acquire, refund,
// due_entries, cleanup_entry, reclaim, read_epoch and hold, which rl_lib runs, and the admin
// scripts admin_start_epoch, admin_status, admin_inspect, admin_restore and admin_clear,
// which rl_admin runs (§A4.1, §A2). A Script carries its name, the lowercase hex SHA-1 of its
// text, which is the name EVALSHA calls it by, and the text SCRIPT LOAD sends when the store
// does not know it yet. Scripts are content-addressed, so two versions never overwrite each
// other (STA-16). The product's Lua lives only here (AGENTS.md §3).
//
// Each script is assembled from its embedded file and the shared prelude.lua (DESIGN §A4.2;
// IMPLEMENTATION_PLAN S4 step 3): its first line (the #!lua header), "\n", the whole
// prelude, then the rest of its file. prelude.lua is never a script itself.
package scripts
