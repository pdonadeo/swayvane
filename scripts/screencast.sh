#!/bin/bash
# Gira da solo le clip del README (assets/demo/*.webp), una dopo l'altra, e
# registra ognuna nel suo file con ~/.local/bin/screencast.fish, nella home:
# <data>_<ora>_<clip>.mp4. Le clip si convertono poi in WebP con ffmpeg
# (1280×720, 20 fps, qualità 75, loop infinito).
#
#   scripts/screencast.sh             tutte le clip tranne la 6 (1 2 3 4 5a 5b)
#   scripts/screencast.sh 4 5a        solo quelle, nell'ordine dato
#   scripts/screencast.sh 6           la clip facoltativa delle regole di Firefox
#   NO_REC=1 scripts/screencast.sh 2  prova senza registrare
#
# È legato alla configurazione dell'autore: screencast.fish va usato in una
# versione senza dialoghi, che parte con i valori dell'ultima volta e la nota
# del nome del file in $SCREENCAST_NOTE e, lanciato di nuovo, ferma e salva;
# le applicazioni delle clip si aprono con i tasti Super+F5…F10 della sua
# configurazione di Sway.
#
# I tasti passano da ydotool, cioè da una tastiera e un mouse virtuali visti dal
# kernel: Sway li tratta come veri e l'overlay dei tasti li mostra. Mentre gira
# non toccare mouse e tastiera; Ctrl+C nel terminale la interrompe e rimette a
# posto quello che ha cambiato (non disturbare, overlay).
#
# Non tocca la configurazione: tasti, icone e regola di Firefox devono essere già
# pronti. I messaggi vanno anche nel journal, perché lo
# script può partire da Sway senza terminale: journalctl -t screencast.
#
# Le finestre aperte restano sui workspace 7, 8 e 9 (6 per la clip 6), che
# all'inizio devono essere vuoti: alla fine vengono chiuse.

set -Eeuo pipefail

BETWEEN=5      # secondi di pausa fra una clip e l'altra
STILL=1.5      # secondi fermi all'inizio e alla fine di ogni clip, per tagliare
OVERLAY_TTL=3  # l'overlay tiene un tasto per 2 s: aspetta che si svuoti

RUNTIME=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
export YDOTOOL_SOCKET=$RUNTIME/.ydotool_socket
SCATTER_JSON=$RUNTIME/swayvane/scatter.json
OVERLAY_SCRIPT=$HOME/.config/sway/scripts/display_keystrokes.sh
SCREENCAST=$HOME/.local/bin/screencast.fish
GSR_RUNDIR=$RUNTIME/screencast   # lo stato che scrive screencast.fish

PHOTOCRAFT=ai.storyteller.photocraft
YAZI=io.github.yazi-rs
FIREFOX_RE='^(firefox|org\.mozilla\.firefox)$'

log() { printf '\e[1;33m[screencast]\e[0m %s\n' "$*" >&2; logger -t screencast -- "$*"; }
die() { printf '\e[1;31m[screencast]\e[0m %s\n' "$*" >&2; logger -t screencast -p user.err -- "$*"; exit 1; }

# ── Tasti ─────────────────────────────────────────────────────────────────────

# Codici dei tasti (linux/input-event-codes.h) che servono alle clip.
declare -A KC=(
    [super]=125 [shift]=42 [ctrl]=29 [alt]=56
    [left]=105 [right]=106 [up]=103 [down]=108 [enter]=28 [space]=57
    [d]=32 [e]=18 [f]=33 [h]=35 [m]=50 [n]=49 [q]=16 [t]=20 [z]=44
    [1]=2 [2]=3 [3]=4 [4]=5 [5]=6 [6]=7 [7]=8 [8]=9 [9]=10 [0]=11
    [f1]=59 [f2]=60 [f3]=61 [f4]=62 [f5]=63 [f6]=64 [f7]=65 [f8]=66 [f9]=67 [f10]=68
)

ydo() { ydotool "$@" > /dev/null || die "ydotool $1 non è andato: ydotoold risponde?"; }

# key Super+Shift+h: preme i tasti nell'ordine e li rilascia al contrario.
key() {
    local -a parts down=() up=()
    local p c
    IFS=+ read -ra parts <<< "$1"
    for p in "${parts[@]}"; do
        c=${KC[${p,,}]:-}
        [[ -n $c ]] || die "tasto sconosciuto: $p"
        down+=("$c:1")
        up=("$c:0" "${up[@]}")
    done
    ydo key -d 30 "${down[@]}" "${up[@]}"
}

release_modifiers() { ydotool key 125:0 42:0 29:0 56:0 > /dev/null 2>&1 || true; }

# ── Albero di Sway ────────────────────────────────────────────────────────────

tree() { swaymsg -t get_tree; }

# Le finestre del workspace numero $1, una per riga: "app_id id".
windows_on() {
    tree | jq -r --arg n "$1" '
        .. | objects | select(.type == "workspace" and (.name | test("^" + $n + "(:|$)")))
        | .. | objects | select((.type == "con" or .type == "floating_con") and .pid != null)
        | "\(.app_id) \(.id)"'
}

focused_app() { tree | jq -r 'first(.. | objects | select(.focused == true)) | .app_id // ""'; }

# rect APP: "x y larghezza altezza" della prima finestra con quell'app_id.
rect() {
    tree | jq -r --arg a "$1" '
        first(.. | objects | select(.app_id? == $a)) | .rect | "\(.x) \(.y) \(.width) \(.height)"'
}

center() { local x y w h; read -r x y w h < <(rect "$1"); echo "$((x + w / 2)) $((y + h / 2))"; }

con_id() { tree | jq -r --arg a "$1" 'first(.. | objects | select(.app_id? == $a)) | .id'; }

# wait_focus APP: aspetta che la finestra appena aperta prenda il focus.
wait_focus() {
    local i
    for ((i = 0; i < 150; i++)); do
        [[ $(focused_app) == "$1" ]] && { sleep 0.4; return; }
        sleep 0.1
    done
    die "$1 non si è aperta (o non ha preso il focus) entro 15 secondi"
}

# open KEY APP: il tasto che apre APP, poi aspetta che sia a fuoco.
open() { key "$1"; wait_focus "$2"; }

# L'editor (avim, su handler_scatter.go) si apre con neo-tree a sinistra, che
# Spazio e chiude.
open_vim() { open Super+F9 vim; sleep 0.5; key Space; key e; sleep 0.3; }

# E si chiude con Spazio Q: un kill chiuderebbe il terminale sotto a nvim, che
# lascerebbe il file di swap e il suo avviso alla volta dopo.
close_vim() { key Space; key Shift+q; }

goto_ws() { swaymsg -q "workspace number $1"; sleep 0.3; }

# fresh N: chiude le finestre aperte dallo script sul workspace N.
fresh() {
    local i id
    [[ -z $(windows_on "$1") ]] && return
    # yazi gira in ghostty, che a un kill chiede conferma prima di chiudere un
    # terminale con un programma attivo: lo si chiude da dentro, con q. Lo
    # stesso per avim, con Spazio Q.
    for id in $(windows_on "$1" | awk -v a="$YAZI" '$1 == a { print $2 }'); do
        swaymsg -q "[con_id=$id] focus"; sleep 0.3
        key q
    done
    for id in $(windows_on "$1" | awk '$1 == "vim" { print $2 }'); do
        swaymsg -q "[con_id=$id] focus"; sleep 0.3
        close_vim
    done
    swaymsg -q "[workspace=\"^$1(:|\$)\"] kill"
    for ((i = 0; i < 100; i++)); do
        [[ -z $(windows_on "$1") ]] && return
        sleep 0.1
    done
    die "sul workspace $1 qualcosa non si chiude (una finestra di dialogo?)"
}

# ── Puntatore ─────────────────────────────────────────────────────────────────

CX=960 CY=540

pointer_set() { CX=$1 CY=$2; swaymsg -q "seat - cursor set $CX $CY"; }

# glide X Y [ms]: porta il puntatore in X,Y con un movimento morbido (accelera
# e rallenta), a passi di `seat cursor set`, che è preciso al pixel.
glide() {
    local steps=40 x y
    local pause
    pause=$(awk -v ms="${3:-800}" -v n=$steps 'BEGIN { printf "%.3f", ms / n / 1000 }')
    while read -r x y; do
        swaymsg -q "seat - cursor set $x $y"
        sleep "$pause"
    done < <(awk -v a="$CX" -v b="$CY" -v c="$1" -v d="$2" -v n=$steps 'BEGIN {
        for (i = 1; i <= n; i++) { t = (1 - cos(3.14159265 * i / n)) / 2
            printf "%d %d\n", a + (c - a) * t, b + (d - b) * t } }')
    CX=$1 CY=$2
}

glide_to() { local x y; read -r x y < <(center "$1"); glide "$x" "$y" "${2:-800}"; }

# Un movimento vero di un pixel avanti e indietro: `seat cursor set` sposta il
# puntatore ma non fa scattare focus_follows_mouse, un evento del mouse sì.
nudge() { ydo mousemove -- 1 0; ydo mousemove -- -1 0; }

click() { ydo click 0xC0; }

# ── Registrazione ─────────────────────────────────────────────────────────────

# screencast.fish suona il conto alla rovescia, lancia gpu-screen-recorder
# staccato ed esce; lanciato mentre registra, lo ferma. Qui si aspetta che il
# processo compaia e, alla fine, che sparisca: il file è chiuso solo allora.

GSR_PID=''

# Il registratore, non le interrogazioni come --list-capture-options che
# screencast.fish lancia prima di partire: è quello con il file di uscita.
# Sulla riga di comando completa, perché il nome del processo è troncato.
gsr_pid() { pgrep --uid "$(id -u)" --full '^gpu-screen-recorder .* -o ' | head -n 1 || true; }

# Un processo terminato resta zombie finché bash non lo raccoglie, e kill -0 lo
# darebbe ancora per vivo.
alive() { local st; st=$(ps -o stat= -p "$1" 2> /dev/null) && [[ $st != Z* ]]; }

rec_start() {
    local fish i
    SCREENCAST_NOTE=$1 "$SCREENCAST" &
    fish=$!
    for ((i = 0; i < 150; i++)); do
        GSR_PID=$(gsr_pid)
        [[ -n $GSR_PID ]] && return
        alive "$fish" || break
        sleep 0.1
    done
    GSR_PID=$(gsr_pid)
    [[ -n $GSR_PID ]] || die "gpu-screen-recorder non è partito (guarda le notifiche e ~/.local/state/screencast/gsr.log)"
}

rec_stop() {
    [[ -n $GSR_PID ]] || return 0
    local out i
    out=$(cat "$GSR_RUNDIR/output" 2> /dev/null || true)
    "$SCREENCAST"
    for ((i = 0; i < 300; i++)); do
        kill -0 "$GSR_PID" 2> /dev/null || break
        sleep 0.1
    done
    kill -0 "$GSR_PID" 2> /dev/null && die "gpu-screen-recorder non si ferma (pid $GSR_PID)"
    GSR_PID=''
    log "salvata: ${out:-?}"
}

# ── Inizio e fine di una clip ─────────────────────────────────────────────────

clip_begin() {
    sleep "$OVERLAY_TTL"
    if [[ -n ${NO_REC:-} ]]; then
        log "clip $1 (prova, non registro)"
    else
        log "clip $1: registro"
        rec_start "$1"
    fi
    sleep "$STILL"
}

clip_end() {
    sleep "$STILL"
    rec_stop
}

# ── Le clip ───────────────────────────────────────────────────────────────────

clip_1() { # icons.webp: la barra che cambia mentre apri e chiudi app
    fresh 7; fresh 8; goto_ws 7
    clip_begin icons
    open Super+Shift+h htop;     sleep 1
    open Super+F8 cava;          sleep 1
    open Super+F7 lazygit;       sleep 1
    key Super+8;                 sleep 0.7
    open Super+F10 $PHOTOCRAFT;  sleep 2
    open Super+Shift+f $YAZI;    sleep 1
    key Super+7;                 sleep 0.8
    [[ $(focused_app) == lazygit ]] || die "sul 7 il focus non è su lazygit"
    key Alt+F4;                  sleep 1.5
    clip_end
}

clip_2() { # autotiling.webp: 30% del lato lungo, tab invece di una fettina
    fresh 7; goto_ws 7
    clip_begin autotiling
    open Super+Shift+h htop;     sleep 1
    open Super+Shift+m mc;       sleep 1
    open Super+F8 cava;          sleep 1
    open_vim;                    sleep 2
    close_vim;                   sleep 1
    clip_end
}

# htop | mc / cava sul 7, focus su cava: lo stato finale della clip 2.
setup_3() {
    local apps
    apps=$(windows_on 7 | cut -d' ' -f1 | sort | tr '\n' ' ')
    if [[ $apps != "cava htop mc " ]]; then
        fresh 7; goto_ws 7
        open Super+Shift+h htop
        open Super+Shift+m mc
        open Super+F8 cava
    else
        goto_ws 7
        swaymsg -q '[app_id="^cava$"] focus'
    fi
    sleep 0.5
}

clip_3() { # maximize.webp
    setup_3
    clip_begin maximize
    sleep 1
    key Super+z;                 sleep 2.5
    key Super+z;                 sleep 1
    clip_end
}

clip_4() { # expose.webp: il giro due volte, così finisce come comincia
    local x y w h
    fresh 8; goto_ws 8
    open Super+F10 $PHOTOCRAFT;  sleep 2
    open Super+Shift+h htop
    key Super+t;                 sleep 0.5
    open Super+Shift+m mc
    open Super+F7 lazygit
    key Super+Left;              sleep 0.3
    key Super+Left;              sleep 0.5
    [[ $(focused_app) == htop ]] || die "sull'8 il tab a fuoco non è htop"
    read -r x y w h < <(rect htop)
    pointer_set $((x + w / 2)) $((y + h / 6))   # nella colonna, dove andrà htop

    clip_begin expose
    sleep 1
    key Super+F5;                sleep 1
    glide_to lazygit 900; nudge; sleep 0.5
    key Super+F5;                sleep 1.5
    key Super+F5;                sleep 1
    glide_to htop 900; nudge;    sleep 0.5
    key Super+F5;                sleep 1
    clip_end
}

# Sei finestre sul 9, tre visibili e tre nascoste: a sinistra tab
# PhotoCraft/vim, in alto a destra stack htop/lazygit, in basso tab mc/cava.
# Focus e puntatore su lazygit.
setup_5() {
    local x y
    if [[ $(windows_on 9 | wc -l) -eq 6 ]]; then
        goto_ws 9
        swaymsg -q '[app_id="^lazygit$"] focus'
    else
        fresh 9; goto_ws 9
        open Super+F10 $PHOTOCRAFT;  sleep 2
        open Super+Shift+h htop
        open Super+Shift+m mc
        open Super+F8 cava
        key Super+Up;                sleep 0.5
        open Super+F7 lazygit
        key Super+Left;              sleep 0.5
        open_vim
        key Super+Right;             sleep 0.5
        [[ $(focused_app) == lazygit ]] || die "sul 9 il focus non è su lazygit"
    fi
    read -r x y < <(center lazygit)
    pointer_set "$x" "$y"
}

clip_5a() { # scatter.webp: clic su htop, poi il numero di lazygit
    local n
    setup_5
    clip_begin scatter
    sleep 1
    key Super+F3;                sleep 1.5
    glide_to $PHOTOCRAFT 700;    sleep 0.3
    glide_to mc 700;             sleep 0.3
    glide_to cava 700;           sleep 0.3
    glide_to htop 700;           sleep 0.4
    click;                       sleep 1.5
    key Super+F3;                sleep 1.5
    n=$(jq --arg id "$(con_id lazygit)" '[.decor[].id | tostring] | index($id) + 1' "$SCATTER_JSON")
    ((n >= 1 && n <= 9)) || die "non trovo il numero di lazygit in $SCATTER_JSON"
    key "$n";                    sleep 1.5
    clip_end
    read -r CX CY < <(center lazygit)   # lo scatter ci ha portato il puntatore
}

clip_5b() { # scatter-grid.webp
    setup_5
    clip_begin scatter-grid
    sleep 1
    key Super+F6;                sleep 2.5
    key Super+F6;                sleep 1
    clip_end
}

clip_6() { # firefox-rules.webp: la regola sul titolo deve essere già nel file
    local ff
    ff=$(tree | jq -r --arg re "$FIREFOX_RE" '
        .. | objects | select(.type == "workspace" and (.name | test("^2(:|$)")))
        | first(.. | objects | select(.app_id? // "" | test($re))) | .id')
    [[ -n $ff ]] || die "non c'è Firefox sul workspace 2"
    fresh 6
    goto_ws 2
    swaymsg -q "[con_id=$ff] focus"; sleep 0.5

    clip_begin firefox-rules
    sleep 1
    key Ctrl+n;                  sleep 1.5
    ydo type -d 60 -H 30 github.com/pdonadeo/swayvane
    sleep 0.5
    key Enter
    local i
    for ((i = 0; i < 150; i++)); do [[ -n $(windows_on 6) ]] && break; sleep 0.1; done
    [[ -n $(windows_on 6) ]] || log "la finestra non è arrivata sul 6: controlla la regola"
    sleep 2
    clip_end
    fresh 6
}

# ── Preparazione e pulizia ────────────────────────────────────────────────────

YDO_PID='' OVERLAY_STARTED='' DND_WAS='' START_WS=''

cleanup() {
    local rc=$?
    release_modifiers
    rec_stop
    if [[ $rc -eq 0 ]]; then
        local n
        for n in "${WORKSPACES[@]}"; do fresh "$n" || true; done
    else
        log "interrotto: le finestre restano sui workspace ${WORKSPACES[*]}"
    fi
    [[ -n $OVERLAY_STARTED ]] && "$OVERLAY_SCRIPT"
    [[ $DND_WAS == false ]] && swaync-client --dnd-off > /dev/null
    [[ -n $YDO_PID ]] && { kill "$YDO_PID" 2> /dev/null; rm -f "$YDOTOOL_SOCKET"; }
    [[ -n $START_WS ]] && swaymsg -q "workspace number $START_WS"
    exit $rc
}

# I workspace che ogni clip usa.
declare -A CLIP_WS=([1]="7 8" [2]=7 [3]=7 [4]=8 [5a]=9 [5b]=9 [6]=6)

main() {
    local -a clips=("$@")
    ((${#clips[@]})) || clips=(1 2 3 4 5a 5b)

    local c n i
    local -a ws
    WORKSPACES=()
    for c in "${clips[@]}"; do
        [[ -n ${CLIP_WS[$c]:-} ]] || die "clip sconosciuta: $c (valide: 1 2 3 4 5a 5b 6)"
        read -ra ws <<< "${CLIP_WS[$c]}"
        WORKSPACES+=("${ws[@]}")
    done
    mapfile -t WORKSPACES < <(printf '%s\n' "${WORKSPACES[@]}" | sort -u)

    for c in ydotool ydotoold jq swaync-client "$SCREENCAST"; do
        command -v $c > /dev/null || die "manca $c"
    done
    pgrep -x swayvane > /dev/null || die "swayvane non sta girando"
    [[ -n ${NO_REC:-} || -z $(gsr_pid) ]] \
        || die "gpu-screen-recorder sta già registrando: fermalo prima (lo script lo avvia e lo ferma a ogni clip)"
    for c in "${WORKSPACES[@]}"; do
        [[ -z $(windows_on "$c") ]] || die "il workspace $c deve essere vuoto"
    done

    START_WS=$(swaymsg -t get_workspaces | jq -r '.[] | select(.focused) | .num')
    trap cleanup EXIT
    trap 'exit 130' INT TERM
    # Con set -e un comando che fallisce chiude lo script senza dire niente.
    trap 'log "errore alla riga $LINENO: $BASH_COMMAND"' ERR

    if ! pgrep -u "$(id -u)" -x ydotoold > /dev/null; then
        rm -f "$YDOTOOL_SOCKET"   # ydotoold non lo toglie quando viene ucciso
        ydotoold --socket-path="$YDOTOOL_SOCKET" --socket-own="$(id -u):$(id -g)" > /dev/null 2>&1 &
        YDO_PID=$!
        sleep 1.5   # Sway deve accorgersi dei dispositivi nuovi
    fi
    ydotool mousemove -- 0 0 > /dev/null 2>&1 \
        || die "ydotool non riesce a parlare con ydotoold ($YDOTOOL_SOCKET)"
    DND_WAS=$(swaync-client --get-dnd)
    swaync-client --dnd-on > /dev/null
    if ! pgrep -x wl-showkeys > /dev/null; then
        "$OVERLAY_SCRIPT"
        OVERLAY_STARTED=1
    fi

    log "clip: ${clips[*]} — non toccare mouse e tastiera"
    for ((i = 0; i < ${#clips[@]}; i++)); do
        c=${clips[i]}
        ((i == 0)) || sleep "$BETWEEN"
        "clip_$c"
        # Ogni clip chiude le sue finestre appena finisce, così la seguente le
        # riapre davvero (PhotoCraft tiene una sola istanza: lanciato di nuovo,
        # passa il file a quella aperta invece di aprire una finestra). Tranne
        # quando la seguente riparte da dove questa è arrivata.
        case "$c ${clips[i + 1]:-}" in
            "2 3" | "5a 5b") ;;
            *) read -ra ws <<< "${CLIP_WS[$c]}"; for n in "${ws[@]}"; do fresh "$n"; done ;;
        esac
    done
    log "fatto"
}

main "$@"
