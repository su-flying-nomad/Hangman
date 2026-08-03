const lobbyPane = document.querySelector("#lobbyPane");
const gamePane = document.querySelector("#gamePane");

const hostName = document.querySelector("#hostName");
const createRoomBtn = document.querySelector("#createRoomBtn");
const roomCreated = document.querySelector("#roomCreated");
const roomCode = document.querySelector("#roomCode");
const shareLink = document.querySelector("#shareLink");
const copyBtn = document.querySelector("#copyBtn");
const qrCanvas = document.querySelector("#qr");

const joinName = document.querySelector("#joinName");
const joinRoom = document.querySelector("#joinRoom");
const joinBtn = document.querySelector("#joinBtn");

const liveRoomCode = document.querySelector("#liveRoomCode");
const liveRound = document.querySelector("#liveRound");
const liveTimer = document.querySelector("#liveTimer");
const liveLives = document.querySelector("#liveLives");
const eventTape = document.querySelector("#eventTape");
const maskedWord = document.querySelector("#maskedWord");
const guessedLetters = document.querySelector("#guessedLetters");
const scoreList = document.querySelector("#scoreList");

let socket;
let localState;
let pendingGuess = false;
let countdownInterval;

function toast(msg) {
  eventTape.textContent = msg;
}

function renderState(state) {
  localState = state;
  liveRoomCode.textContent = state.roomId;
  
  clearInterval(countdownInterval);

  if (state.isGameOver) {
    liveRound.textContent = "Finished";
    liveTimer.textContent = "0s";
    liveLives.textContent = "-";
    maskedWord.textContent = "GAME OVER";
    maskedWord.style.color = "inherit";
    guessedLetters.textContent = "All rounds completed.";
    eventTape.textContent = "The game has ended.";
  } else if (state.round === 0) {
    // NEW: Waiting state before enough players join
    liveRound.textContent = "Waiting";
    liveTimer.textContent = "∞";
    liveLives.textContent = "-";
    maskedWord.textContent = "WAITING FOR PLAYERS";
    maskedWord.style.color = "inherit";
    guessedLetters.textContent = "Game starts when someone else joins...";
    eventTape.textContent = "Invite a friend with the link or room code!";
  } else {
    // Normal game loop
    liveRound.textContent = `${state.round} / ${state.totalRounds}`;
    liveLives.textContent = String(state.maxWrong - state.wrongGuesses);
    guessedLetters.textContent = state.guessed.length ? state.guessed.join(" ") : "None yet";
    
    // ... rest of the existing timer and rendering logic ...

    const updateTimer = () => {
      const now = Math.floor(Date.now() / 1000);
      const rem = Math.max(0, state.roundEndTime - now);
      liveTimer.textContent = rem + "s";
    };
    updateTimer(); 
    countdownInterval = setInterval(updateTimer, 1000);

    if (state.playerStatus === "solved") {
      maskedWord.textContent = state.answer;
      maskedWord.style.color = "var(--ok)";
      eventTape.textContent = "You cracked it! Waiting for others to finish...";
    } else if (state.playerStatus === "failed") {
      maskedWord.textContent = state.answer;
      maskedWord.style.color = "var(--danger)";
      eventTape.textContent = "Out of lives! Waiting for the next round...";
    } else {
      maskedWord.textContent = state.maskedWord;
      maskedWord.style.color = "inherit";
      eventTape.textContent = "Round in progress...";
    }
  }

  scoreList.innerHTML = "";
  for (const p of state.players) {
    const li = document.createElement("li");
    let icon = "";
    if (p.status === "solved") icon = " ✅";
    if (p.status === "failed") icon = " ❌";
    
    li.innerHTML = `<span>${p.name}${icon}</span><strong>${p.score}</strong>`;
    scoreList.appendChild(li);
  }

  pendingGuess = false;
}

function wsUrl(roomId, name) {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  return `${proto}://${location.host}/ws?room=${encodeURIComponent(roomId)}&name=${encodeURIComponent(name)}`;
}

function connect(roomId, name) {
  socket = new WebSocket(wsUrl(roomId, name));

  socket.addEventListener("open", () => {
    lobbyPane.classList.add("hidden");
    gamePane.classList.remove("hidden");
    toast("Connected. Waiting for state...");
  });

  socket.addEventListener("message", (ev) => {
    const msg = JSON.parse(ev.data);
    if (msg.type === "state") {
      renderState(msg.payload);
    } else if (msg.type === "toast") {
      toast(msg.payload.message || "Notice");
    }
  });

  socket.addEventListener("close", () => {
    toast("Disconnected. Refresh to reconnect.");
    pendingGuess = false;
  });

  socket.addEventListener("error", () => {
    toast("Connection error. Please retry.");
    pendingGuess = false;
  });
}

function sendGuess(letter) {
  if (!socket || socket.readyState !== WebSocket.OPEN || pendingGuess) {
    return;
  }

  if (localState?.guessed?.includes(letter)) {
    return;
  }

  pendingGuess = true;
  socket.send(
    JSON.stringify({
      type: "guess",
      letter,
    }),
  );
}

async function createRoomAndJoin() {
  const name = hostName.value.trim();
  if (!name) {
    alert("Enter your name first.");
    return;
  }

  const res = await fetch("/api/rooms", {
    method: "POST",
  });

  if (!res.ok) {
    alert("Could not create room.");
    return;
  }

  const data = await res.json();
  roomCode.textContent = data.roomId;
  shareLink.value = data.joinUrl;
  roomCreated.classList.remove("hidden");

  window.QRCode.toCanvas(
    qrCanvas,
    data.joinUrl,
    {
      width: 160,
      margin: 1,
      color: { dark: "#000000", light: "#ffffff" },
    },
    () => {},
  );

  joinRoom.value = data.roomId;
  joinName.value = name;

  const url = new URL(location.href);
  url.searchParams.set("room", data.roomId);
  history.replaceState({}, "", url);

  connect(data.roomId, name);
}

function joinExisting() {
  const name = joinName.value.trim();
  const roomId = joinRoom.value.trim().toUpperCase();
  if (!name || !roomId) {
    alert("Name and room code are required.");
    return;
  }

  const url = new URL(location.href);
  url.searchParams.set("room", roomId);
  history.replaceState({}, "", url);

  connect(roomId, name);
}

copyBtn?.addEventListener("click", async () => {
  if (!shareLink.value) {
    return;
  }
  await navigator.clipboard.writeText(shareLink.value);
  copyBtn.textContent = "Copied";
  setTimeout(() => {
    copyBtn.textContent = "Copy";
  }, 1000);
});

createRoomBtn.addEventListener("click", createRoomAndJoin);
joinBtn.addEventListener("click", joinExisting);

window.addEventListener("keydown", (ev) => {
  const active = document.activeElement;
  const typingInInput = active && (active.tagName === "INPUT" || active.tagName === "TEXTAREA");
  if (typingInInput) {
    return;
  }

  const key = ev.key.toUpperCase();
  if (key.length !== 1 || key < "A" || key > "Z") {
    return;
  }

  ev.preventDefault();
  sendGuess(key);
});

const maybeRoom = new URLSearchParams(location.search).get("room");
if (maybeRoom) {
  joinRoom.value = maybeRoom.toUpperCase();
}