const errorMessages = {
  login: {
    invalid: "The username or password did not match. Try again.",
    busy: "Too many sign-in attempts are in progress. Wait a moment and try again.",
    csrf_expired: "This form belongs to an earlier browser session. Try again with the refreshed form below.",
    csrf_missing: "This form was missing its security token. Try again with the refreshed form below.",
    csrf_mismatch: "This form was out of date. Try again with the refreshed form below.",
    origin: "This form could not be verified as same-origin. Reload the page and try again.",
  },
  setup: {
    invalid: "The setup form could not be read. Please try again.",
    username: "Choose a username with 3–64 supported characters.",
    password: "Use a password between 12 and 256 characters.",
    mismatch: "The two passwords do not match.",
    busy: "Another setup attempt is in progress. Wait a moment and try again.",
    csrf_expired: "This form belongs to an earlier browser session. Try again with the refreshed form below.",
    csrf_missing: "This form was missing its security token. Try again with the refreshed form below.",
    csrf_mismatch: "This form was out of date. Try again with the refreshed form below.",
    origin: "This form could not be verified as same-origin. Reload the page and try again.",
  },
};

for (const button of document.querySelectorAll("[data-reveal]")) {
  button.addEventListener("click", () => {
    const input = document.getElementById(button.dataset.reveal);
    const showing = input.type === "text";
    input.type = showing ? "password" : "text";
    const label = showing ? "Show password" : "Hide password";
    button.setAttribute("aria-label", label);
    button.title = label;
    input.focus();
  });
}

const page = document.body.dataset.page;
const errorCode = document.body.dataset.error || new URLSearchParams(location.search).get("error");
const error = document.getElementById("error");
if (error && errorMessages[page] && errorMessages[page][errorCode]) {
  error.textContent = errorMessages[page][errorCode];
  error.classList.add("visible");
}
