window.addEventListener("DOMContentLoaded", () => {
  renderMathInElement(
    document.body,
    {
      delimiters: [
        { left: "$$", right: "$$", display: true },
        { left: "\\[", right: "\\]", display: true },
        { left: "$", right: "$", display: false },
        { left: "\\(", right: "\\)", display: false }
      ]
    }
  );

  new Zooming({
    bgColor: '#000',
    bgOpacity: '0.9',
    closeOnWindowResize: true,
  }).listen("img");
})