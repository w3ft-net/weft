package shellcore

var helpText = DimText("weft shell") + "\r\n\r\n" +

	AccentText("Querying logs") + "\r\n" +
	"  cat /log/<app>.log                   show all logs for an app\r\n" +
	"  cat /log/api.log | grep ERROR        filter with a pipeline\r\n" +
	"  cat /log/api.log | grep ERROR | wc -l\r\n" +
	"  tail /log/api.log | head -20         last 20 lines\r\n" +
	"\r\n" +

	AccentText("Filesystem") + "\r\n" +
	"  ls                                   list directories\r\n" +
	"  ls /log                              list available log sources\r\n" +
	"  ls /bin                              list saved functions\r\n" +
	"  ls /cron.d                           list scheduled watches\r\n" +
	"\r\n" +

	AccentText("Functions (/bin/)") + "\r\n" +
	"  vi /bin/check_errors                 create/edit a function\r\n" +
	"  cat /bin/check_errors                view source\r\n" +
	"  check_errors                         run it (resolves from /bin/)\r\n" +
	"  rm /bin/check_errors                 delete it\r\n" +
	"\r\n" +

	AccentText("Watches (/cron.d/)") + "\r\n" +
	"  vi /cron.d/error-check               create/edit a watch\r\n" +
	"  cat /cron.d/error-check              view definition\r\n" +
	"  rm /cron.d/error-check               delete (stops the watch)\r\n" +
	"\r\n" +
	"  Watch file format:\r\n" +
	"    # schedule: every 1m               or cron: */5 * * * *\r\n" +
	"    # notify: slack:#ops               optional — alert on stderr\r\n" +
	"    cat /log/api.log | grep ERROR | wc -l\r\n" +
	"\r\n" +
	"  The pipeline runs on the schedule. Its stdout is recorded.\r\n" +
	"  If the pipeline writes to stderr, a notification is sent.\r\n" +
	"\r\n" +
	"  Tier limits:\r\n" +
	"    trial: no watches    swatch: 5 watches, min 5m interval\r\n" +
	"    yard:  10 watches    bolt:   50 watches (min 1m interval)\r\n" +
	"\r\n" +

	AccentText("Editing (vim)") + "\r\n" +
	"  i        enter insert mode            :w       save\r\n" +
	"  Esc      back to normal mode          :wq      save and quit\r\n" +
	"  dd       delete line                  :q       quit (discard)\r\n" +
	"\r\n" +

	AccentText("Alerting") + "\r\n" +
	"  <pipeline> | alert above 50           threshold alert\r\n" +
	"  <pipeline> | alert above 50 --duration 5m   sustained\r\n" +
	"  <pipeline> | alert changed             value changed\r\n" +
	"  <pipeline> | alert rising 100%          rate of change\r\n" +
	"  man alert                              full documentation\r\n" +
	"\r\n" +

	AccentText("Other") + "\r\n" +
	"  which <name>                         show where a command lives\r\n" +
	"  type <name>                          identify a command\r\n" +
	"  apps                                 list available apps\r\n" +
	"  clear                                clear the screen\r\n" +
	"  exit                                 quit the shell\r\n"
