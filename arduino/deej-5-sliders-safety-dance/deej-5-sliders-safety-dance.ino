const String DEVICE_ID = "DEEJ_SAFETY_DANCE_V1.0";
const int NUM_SLIDERS = 5;
const int NOISE_THRESHOLD_PCT = 1;
const int NOISE_THRESHOLD = (1024 / 100) * NOISE_THRESHOLD_PCT;
const int CONN_BAUD_RATE = 9600;
const unsigned long SEND_INTERVAL = 50;
const unsigned long CONN_START_WAIT_INTERVAL = 5000;
const int analogInputs[NUM_SLIDERS] = {A0, A1, A2, A3, A4};

int sliderValues[NUM_SLIDERS];
int lastSentSliderValues[NUM_SLIDERS];
unsigned long lastSendTime = 0;


void setup() { 
  for (int i = 0; i < NUM_SLIDERS; i++) {
    pinMode(analogInputs[i], INPUT);
    lastSentSliderValues[i] = -1;
  }

  tryConnect();
}

void tryConnect() {
  Serial.begin(CONN_BAUD_RATE);

  unsigned long startTime = millis();
  while (!Serial && (millis() - startTime < CONN_START_WAIT_INTERVAL)) {
    delay(100);
  }

  if (Serial) {
    Serial.print("ID:");
    Serial.println(DEVICE_ID);
  }
}

void loop() {
  if (Serial) {
    unsigned long currentTime = millis();

    updateSliderValues();
    // printSliderValues(); // For debug

    if (currentTime - lastSendTime >= SEND_INTERVAL) {
      if (valuesChanged()) {
        sendSliderValues();
        lastSendTime = currentTime;
      }
    }
  } else {
    tryConnect();
  }

  delay(10);
}

void updateSliderValues() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
     sliderValues[i] = analogRead(analogInputs[i]);
  }
}

bool valuesChanged() {
  for (int i = 0; i < NUM_SLIDERS; i++) {
    if (abs(sliderValues[i] - lastSentSliderValues[i]) > NOISE_THRESHOLD) {
      return true;
    }
  }
  return false;
}

void sendSliderValues() {
  if (!Serial) return;

  String builtString = String("");

  for (int i = 0; i < NUM_SLIDERS; i++) {
    builtString += String((int)sliderValues[i]);
    lastSentSliderValues[i] = sliderValues[i];

    if (i < NUM_SLIDERS - 1) {
      builtString += String("|");
    }
  }
  
  Serial.println(builtString);
  Serial.flush();
}

void printSliderValues() {
  if (!Serial) return;

  for (int i = 0; i < NUM_SLIDERS; i++) {
    String printedString = String("Slider #") + String(i + 1) + String(": ") + String(sliderValues[i]) + String(" mV");
    Serial.write(printedString.c_str());

    if (i < NUM_SLIDERS - 1) {
      Serial.write(" | ");
    } else {
      Serial.write("\n");
    }
  }
}
