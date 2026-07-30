// ============================================================
// 看家狗本地行为：外围通道导航 + 随机走停 + 姿势动画
// ============================================================

export const DOG_STATES = Object.freeze({
  TURN: 'turn',
  WALK: 'walk',
  IDLE: 'idle',
  SNIFF: 'sniff',
  SIT: 'sit',
  LIE: 'lie',
  HUNGRY_REST: 'hungry-rest',
});

const HALF_X = 15.5;
const HALF_Z = 8.5;
const EDGE_X = HALF_X * 2;
const EDGE_Z = HALF_Z * 2;
const PATH_LENGTH = EDGE_X * 2 + EDGE_Z * 2;
const HUNGRY_PATH_DISTANCE = EDGE_X + EDGE_Z + 3;
const MAX_DT = 0.1;
const ROUTE_STEP = 2;
const LANE_VARIANCE = 0.38;
const TURN_SPEED = 2.8;
const PATH_CORNERS = [0, EDGE_X, EDGE_X + EDGE_Z, EDGE_X * 2 + EDGE_Z, PATH_LENGTH];

const wrapPathDistance = (distance) => (
  ((distance % PATH_LENGTH) + PATH_LENGTH) % PATH_LENGTH
);

const clampRandom = (random) => {
  const value = Number(random());
  return Number.isFinite(value) ? Math.min(0.999999, Math.max(0, value)) : 0.5;
};

const randomBetween = (random, min, max) => min + (max - min) * clampRandom(random);

const angleDelta = (from, to) => {
  let delta = (to - from) % (Math.PI * 2);
  if (delta > Math.PI) delta -= Math.PI * 2;
  if (delta < -Math.PI) delta += Math.PI * 2;
  return delta;
};

const damp = (current, target, lambda, dt) => (
  current + (target - current) * (1 - Math.exp(-lambda * dt))
);

/** 外围通道中心线上的坐标；距离按顺时针从左下角开始。 */
export function dogPathPoint(distance, laneOffset = 0) {
  let value = wrapPathDistance(distance);
  const lane = Math.max(-LANE_VARIANCE, Math.min(LANE_VARIANCE, Number(laneOffset) || 0));
  if (value <= EDGE_X) return { x: -HALF_X + value, z: -HALF_Z - lane };
  value -= EDGE_X;
  if (value <= EDGE_Z) return { x: HALF_X + lane, z: -HALF_Z + value };
  value -= EDGE_Z;
  if (value <= EDGE_X) return { x: HALF_X - value, z: HALF_Z + lane };
  value -= EDGE_X;
  return { x: -HALF_X - lane, z: HALF_Z - value };
}

/** 测试与调试使用：确保点在围栏内并处于田地整体包围盒之外。 */
export function isDogWalkablePoint(point) {
  if (!point || !Number.isFinite(point.x) || !Number.isFinite(point.z)) return false;
  const insideFence = Math.abs(point.x) <= 16.05 && Math.abs(point.z) <= 9.05;
  const outsidePlots = Math.abs(point.x) >= 15 || Math.abs(point.z) >= 8;
  return insideFence && outsidePlots;
}

function nearestPathDistance(point) {
  const candidates = [
    { distance: Math.abs(point.z + HALF_Z), value: Math.min(EDGE_X, Math.max(0, point.x + HALF_X)) },
    { distance: Math.abs(point.x - HALF_X), value: EDGE_X + Math.min(EDGE_Z, Math.max(0, point.z + HALF_Z)) },
    { distance: Math.abs(point.z - HALF_Z), value: EDGE_X + EDGE_Z + Math.min(EDGE_X, Math.max(0, HALF_X - point.x)) },
    { distance: Math.abs(point.x + HALF_X), value: EDGE_X * 2 + EDGE_Z + Math.min(EDGE_Z, Math.max(0, HALF_Z - point.z)) },
  ];
  candidates.sort((a, b) => a.distance - b.distance);
  return wrapPathDistance(candidates[0].value);
}

function buildRoute(startDistance, direction, travelDistance, laneOffset = 0) {
  const route = [];
  let cursor = wrapPathDistance(startDistance);
  let remaining = Math.max(0, travelDistance);
  while (remaining > 1e-6) {
    let cornerDistance;
    if (direction > 0) {
      const nextCorner = PATH_CORNERS.find((corner) => corner > cursor + 1e-6) ?? PATH_LENGTH;
      cornerDistance = nextCorner - cursor;
    } else {
      const previousCorners = PATH_CORNERS.filter((corner) => corner < cursor - 1e-6);
      const previousCorner = previousCorners.at(-1) ?? (PATH_CORNERS.at(-2) - PATH_LENGTH);
      cornerDistance = cursor - previousCorner;
    }
    const step = Math.min(ROUTE_STEP, remaining, cornerDistance);
    cursor = wrapPathDistance(cursor + direction * step);
    route.push({ ...dogPathPoint(cursor, laneOffset), pathDistance: cursor });
    remaining -= step;
  }
  return route;
}

function applyDogPose(rig, state, time, dt, speed) {
  if (!rig) return null;
  const walking = state === DOG_STATES.WALK;
  const trotting = walking && speed > 1.7;
  const strideRate = trotting ? 12 : 8;
  const stride = walking ? Math.sin(time * strideRate) * (trotting ? 0.72 : 0.5) : 0;
  const breathing = Math.sin(time * 2.1) * 0.012;

  let rootY = walking ? Math.abs(Math.sin(time * strideRate)) * 0.052 : breathing;
  let rootTilt = 0;
  let headDown = 0;
  let headLook = 0;
  let headForward = 0;
  let frontTargets = [0, 0];
  let hindTargets = [0, 0];
  let tailWag = Math.sin(time * 3.2) * 0.18;
  let tailDrop = 0;

  if (state === DOG_STATES.IDLE) {
    headLook = Math.sin(time * 1.15) * 0.24;
    rootTilt = Math.sin(time * 1.15) * 0.025;
  } else if (state === DOG_STATES.SNIFF) {
    const sniffPulse = Math.sin(time * 5.2) * 0.022;
    rootY = -0.012 + sniffPulse;
    rootTilt = -0.07;
    headDown = -0.76 + sniffPulse;
    headLook = Math.sin(time * 2) * 0.08;
    headForward = 0.075 + sniffPulse;
    frontTargets = [-0.13, -0.13];
    hindTargets = [0.08, 0.08];
    tailWag = Math.sin(time * 2.4) * 0.12;
  } else if (state === DOG_STATES.SIT) {
    rootY = -0.12;
    rootTilt = -0.09;
    frontTargets = [-0.14, -0.14];
    hindTargets = [-1.15, -1.15];
    headLook = Math.sin(time * 0.85) * 0.15;
    tailWag = Math.sin(time * 2) * 0.12;
    tailDrop = -0.18;
  } else if (state === DOG_STATES.LIE || state === DOG_STATES.HUNGRY_REST) {
    rootY = -0.25 + breathing;
    headDown = state === DOG_STATES.HUNGRY_REST ? -0.28 : -0.12;
    headForward = state === DOG_STATES.HUNGRY_REST ? 0.035 : 0;
    frontTargets = [1.05, 1.05];
    hindTargets = [-1.05, -1.05];
    tailWag = state === DOG_STATES.HUNGRY_REST ? 0 : Math.sin(time * 1.4) * 0.07;
    tailDrop = -0.5;
  } else if (state === DOG_STATES.TURN) {
    tailWag = Math.sin(time * 3) * 0.15;
  }

  if (walking) {
    frontTargets = [stride, -stride];
    hindTargets = [-stride, stride];
  }

  rig.root.position.y = damp(rig.root.position.y, rootY, 14, dt);
  rig.root.rotation.z = damp(rig.root.rotation.z, rootTilt, 14, dt);
  rig.root.scale.y = damp(rig.root.scale.y, 1 + (walking ? 0 : breathing), 12, dt);
  rig.head.position.x = damp(rig.head.position.x, (rig.head.userData.baseX ?? rig.head.position.x) + headForward, 14, dt);
  rig.head.rotation.z = damp(rig.head.rotation.z, headDown, 14, dt);
  rig.head.rotation.y = damp(rig.head.rotation.y, headLook, 12, dt);
  rig.tail.rotation.y = damp(rig.tail.rotation.y, tailWag, 14, dt);
  rig.tail.rotation.z = damp(rig.tail.rotation.z, tailDrop, 12, dt);

  rig.frontLegs.forEach((leg, index) => {
    leg.rotation.z = damp(leg.rotation.z, frontTargets[index], 16, dt);
  });
  rig.hindLegs.forEach((leg, index) => {
    leg.rotation.z = damp(leg.rotation.z, hindTargets[index], 16, dt);
  });

  return {
    rootY,
    rootTilt,
    headDown,
    headLook,
    headForward,
    frontTargets,
    hindTargets,
    tailWag,
    tailDrop,
    stride,
  };
}

export class DogBehaviorController {
  constructor({ random = Math.random } = {}) {
    this.random = random;
    this.pathDistance = clampRandom(random) * PATH_LENGTH;
    this.position = dogPathPoint(this.pathDistance);
    this.heading = 0;
    this.state = DOG_STATES.IDLE;
    this.stateTime = 0;
    this.stateDuration = randomBetween(random, 1.5, 4);
    this.route = [];
    this.speed = 0;
    this.hungry = false;
    this.time = 0;
    this.lastDirection = 0;
    this.laneOffset = 0;
  }

  attach(dog) {
    dog.position.set(this.position.x, 0, this.position.z);
    dog.rotation.set(0, this.heading, 0);
  }

  _targetHeading() {
    const target = this.route[0];
    if (!target) return this.heading;
    return Math.atan2(-(target.z - this.position.z), target.x - this.position.x);
  }

  _beginTurn() {
    if (!this.route.length) return;
    this.state = DOG_STATES.TURN;
    this.stateTime = 0;
  }

  _beginRandomTrip() {
    const direction = this.lastDirection === 0
      ? (clampRandom(this.random) < 0.5 ? -1 : 1)
      : (clampRandom(this.random) < 0.35 ? -this.lastDirection : this.lastDirection);
    const distance = randomBetween(this.random, 3, 10);
    const trot = clampRandom(this.random) < 0.12;
    this.speed = trot ? 1.9 : randomBetween(this.random, 1, 1.5);
    this.lastDirection = direction;
    this.laneOffset = randomBetween(this.random, -LANE_VARIANCE, LANE_VARIANCE);
    this.route = buildRoute(this.pathDistance, direction, distance, this.laneOffset);
    this._beginTurn();
  }

  _beginHungryReturn() {
    this.pathDistance = nearestPathDistance(this.position);
    const clockwise = wrapPathDistance(HUNGRY_PATH_DISTANCE - this.pathDistance);
    const counterClockwise = PATH_LENGTH - clockwise;
    const direction = clockwise <= counterClockwise ? 1 : -1;
    const distance = Math.min(clockwise, counterClockwise);
    this.speed = 1.45;
    this.laneOffset = 0;
    this.route = buildRoute(this.pathDistance, direction, distance);
    if (this.route.length) this._beginTurn();
    else this._setHungryRest();
  }

  _setHungryRest() {
    this.state = DOG_STATES.HUNGRY_REST;
    this.stateTime = 0;
    this.stateDuration = Number.POSITIVE_INFINITY;
    this.route = [];
    this.pathDistance = HUNGRY_PATH_DISTANCE;
    this.laneOffset = 0;
    this.position = dogPathPoint(HUNGRY_PATH_DISTANCE);
  }

  _chooseRest() {
    const pick = clampRandom(this.random);
    this.stateTime = 0;
    if (pick < 0.35) {
      this.state = DOG_STATES.IDLE;
      this.stateDuration = randomBetween(this.random, 1.5, 4);
    } else if (pick < 0.65) {
      this.state = DOG_STATES.SNIFF;
      this.stateDuration = randomBetween(this.random, 1.5, 2.8);
    } else if (pick < 0.9) {
      this.state = DOG_STATES.SIT;
      this.stateDuration = randomBetween(this.random, 3, 6);
    } else {
      this.state = DOG_STATES.LIE;
      this.stateDuration = randomBetween(this.random, 5, 10);
    }
  }

  _advanceWalk(dt) {
    const target = this.route[0];
    if (!target) {
      if (this.hungry) this._setHungryRest();
      else this._chooseRest();
      return;
    }

    const dx = target.x - this.position.x;
    const dz = target.z - this.position.z;
    const distance = Math.hypot(dx, dz);
    const step = this.speed * dt;
    if (distance <= step || distance < 1e-6) {
      this.position = { x: target.x, z: target.z };
      this.pathDistance = target.pathDistance;
      this.route.shift();
      if (!this.route.length) {
        if (this.hungry) this._setHungryRest();
        else this._chooseRest();
        return;
      }
      if (Math.abs(angleDelta(this.heading, this._targetHeading())) > 0.12) this._beginTurn();
      return;
    }
    this.position.x += (dx / distance) * step;
    this.position.z += (dz / distance) * step;
  }

  update(dog, dt, elapsed, hungry = false) {
    const safeDt = Math.max(0, Math.min(Number(dt) || 0, MAX_DT));
    this.time = Number.isFinite(elapsed) ? elapsed : this.time + safeDt;

    if (hungry !== this.hungry) {
      this.hungry = hungry;
      if (hungry) {
        this._beginHungryReturn();
      } else {
        this.state = DOG_STATES.IDLE;
        this.stateTime = 0;
        this.stateDuration = randomBetween(this.random, 0.8, 1.8);
      }
    }

    if (this.state === DOG_STATES.TURN) {
      const targetHeading = this._targetHeading();
      const delta = angleDelta(this.heading, targetHeading);
      const maxTurn = 3.2 * safeDt;
      this.heading += Math.max(-maxTurn, Math.min(maxTurn, delta));
      if (Math.abs(delta) < 0.05) {
        this.heading = targetHeading;
        this.state = DOG_STATES.WALK;
        this.stateTime = 0;
      }
    } else if (this.state === DOG_STATES.WALK) {
      this._advanceWalk(safeDt);
    } else if (this.state !== DOG_STATES.HUNGRY_REST) {
      this.stateTime += safeDt;
      if (this.stateTime >= this.stateDuration) this._beginRandomTrip();
    }

    dog.position.set(this.position.x, 0, this.position.z);
    dog.rotation.y = this.heading;
    dog.rotation.z = 0;
    const pose = applyDogPose(dog.userData.rig, this.state, this.time, safeDt, this.speed);
    dog.userData.behaviorState = this.state;
    dog.userData.behaviorPose = pose;
    return {
      position: { ...this.position },
      heading: this.heading,
      state: this.state,
      speed: this.speed,
      pose,
    };
  }
}
